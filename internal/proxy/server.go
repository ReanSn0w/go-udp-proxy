package proxy

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const SOL_UDP = 17 // Linux

type readResult struct {
	n   int
	err error
}

type Options struct {
	BufferSize       int  `long:"buffer-size" env:"BUFFER_SIZE" default:"65507" description:"Максимальный размер UDP пакета"`
	ReadDeadline     int  `long:"read-deadline" env:"READ_DEADLINE" default:"100" description:"Время ожидания ответа, мс"`
	CleanupInterval  int  `long:"cleanup-interval" env:"CLEANUP_INTERVAL" default:"30" description:"Интервал закрытия неактивных соединений, c"`
	InactiveTimeout  int  `long:"inactive-timeout" env:"INACTIVE_TIMEOUT" default:"5" description:"Время по прошествии которого клиент считается неактивным, м"`
	StatsInterval    int  `long:"stats-interval" env:"STATS_INTERVAL" default:"30" description:"Интервал вывода статистики, c"`
	WorkerCount      int  `long:"worker-count" env:"WORKER_COUNT" default:"0" description:"Количество рабочих (0 = автоматически)"`
	MaxConnections   int  `long:"max-connections" env:"MAX_CONNECTIONS" default:"10000" description:"Максимальное количество одновременных соединений"`
	SocketBufferSize int  `long:"socket-buffer-size" env:"SOCKET_BUFFER_SIZE" default:"4194304" description:"Размер UDP socket buffer (4MB)"`
	EnableGRO        bool `long:"enable-gro" env:"ENABLE_GRO" default:"false" description:"Включить Generic Receive Offload"`
}

// Server представляет UDP прокси сервер
type Server struct {
	opts       Options
	listenAddr string
	listenHost string
	listenPort int
	targetAddr string
	targetHost string
	targetPort int
	conn       *net.UDPConn
	clients    map[string]*clientConn
	mu         sync.RWMutex
	logger     Logger

	// Graceful shutdown
	done         chan struct{}
	shutdownOnce sync.Once
	shutdownCh   chan struct{}

	// Статистика
	packetsProcessed atomic.Int64
	bytesForwarded   atomic.Int64
	activeClients    atomic.Int32
	packetDropped    atomic.Int64
}

// clientConn представляет соединение с клиентом
type clientConn struct {
	addr       *net.UDPAddr
	targetConn *net.UDPConn
	lastSeen   time.Time
	cancel     context.CancelFunc
	done       chan struct{}
	wg         sync.WaitGroup
	// Буфер для отправки пакетов
	sendQueue chan []byte
}

// Logger интерфейс для логирования
type Logger interface {
	Logf(format string, args ...interface{})
}

// NewServer создает новый UDP прокси сервер
func NewServer(
	listenHost string,
	listenPort int,
	targetHost string,
	targetPort int,
	logger Logger,
	opts Options,
) *Server {
	if opts.WorkerCount <= 0 {
		opts.WorkerCount = 16
	}
	if opts.MaxConnections <= 0 {
		opts.MaxConnections = 10000
	}
	if opts.BufferSize <= 0 {
		opts.BufferSize = 65507
	}
	if opts.SocketBufferSize <= 0 {
		opts.SocketBufferSize = 4 * 1024 * 1024
	}
	if opts.CleanupInterval <= 0 {
		opts.CleanupInterval = 30
	}
	if opts.InactiveTimeout <= 0 {
		opts.InactiveTimeout = 5
	}
	if opts.ReadDeadline <= 0 {
		opts.ReadDeadline = 100
	}
	if opts.StatsInterval <= 0 {
		opts.StatsInterval = 30
	}

	return &Server{
		opts:       opts,
		listenHost: listenHost,
		listenPort: listenPort,
		listenAddr: fmt.Sprintf("%s:%d", listenHost, listenPort),
		targetHost: targetHost,
		targetPort: targetPort,
		targetAddr: fmt.Sprintf("%s:%d", targetHost, targetPort),
		clients:    make(map[string]*clientConn),
		logger:     logger,
		done:       make(chan struct{}),
		shutdownCh: make(chan struct{}), // ✅
	}
}

// Start запускает UDP прокси сервер
func (s *Server) Start(ctx context.Context) error {
	s.logger.Logf("[DEBUG] Start: begin")
	listenAddr, err := net.ResolveUDPAddr("udp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to resolve listen address: %w", err)
	}

	s.conn, err = net.ListenUDP("udp", listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on UDP: %w", err)
	}

	if err := s.optimizeListenSocket(); err != nil {
		s.logger.Logf("[WARN] Failed to optimize listen socket: %v", err)
	}

	s.logger.Logf("[INFO] UDP Proxy started on %s -> %s", s.listenAddr, s.targetAddr)

	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	go s.cleanupConnections(bgCtx)
	go s.statsLogger(bgCtx)

	s.logger.Logf("[DEBUG] Start: calling handlePackets")
	err = s.handlePackets(ctx)
	s.logger.Logf("[DEBUG] Start: handlePackets returned: %v", err)

	s.logger.Logf("[DEBUG] Start: closing connection immediately")
	s.closeConnection()
	s.logger.Logf("[DEBUG] Start: closeConnection() returned")

	s.logger.Logf("[DEBUG] Start: waiting for all clients to finish")

	s.mu.RLock()
	clients := make([]*clientConn, 0, len(s.clients))
	for _, client := range s.clients {
		clients = append(clients, client)
	}
	s.mu.RUnlock()

	for i, client := range clients {
		s.logger.Logf("[DEBUG] Start: cancelling client %d", i)
		client.cancel()
	}

	for i, client := range clients {
		s.logger.Logf("[DEBUG] Start: waiting for client %d", i)
		client.wg.Wait()
		s.logger.Logf("[DEBUG] Start: client %d done", i)
	}

	s.logger.Logf("[DEBUG] Start: all clients finished")
	s.Stop()
	s.logger.Logf("[DEBUG] Start: Stop() returned")

	return err
}

// optimizeListenSocket оптимизирует параметры UDP сокета
func (s *Server) optimizeListenSocket() error {
	file, err := s.conn.File()
	if err != nil {
		return err
	}
	defer file.Close()

	fd := int(file.Fd())

	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, s.opts.SocketBufferSize); err != nil {
		return fmt.Errorf("failed to set SO_RCVBUF: %w", err)
	}

	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, s.opts.SocketBufferSize); err != nil {
		return fmt.Errorf("failed to set SO_SNDBUF: %w", err)
	}

	if s.opts.EnableGRO {
		if err := syscall.SetsockoptInt(fd, SOL_UDP, 104, 1); err != nil {
			s.logger.Logf("[WARN] Failed to enable UDP GRO: %v", err)
		}
	}

	s.logger.Logf("[INFO] Socket buffer optimized: %d bytes", s.opts.SocketBufferSize)
	return nil
}

// optimizeTargetSocket оптимизирует параметры целевого соединения
func (s *Server) optimizeTargetSocket(conn *net.UDPConn) error {
	file, err := conn.File()
	if err != nil {
		return err
	}
	defer file.Close()

	fd := int(file.Fd())

	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, s.opts.SocketBufferSize); err != nil {
		return fmt.Errorf("failed to set SO_SNDBUF: %w", err)
	}

	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, s.opts.SocketBufferSize); err != nil {
		return fmt.Errorf("failed to set SO_RCVBUF: %w", err)
	}

	if s.opts.EnableGRO {
		if err := syscall.SetsockoptInt(fd, SOL_UDP, 104, 1); err != nil {
			s.logger.Logf("[WARN] Failed to enable UDP GSO: %v", err)
		}
	}

	return nil
}

// handlePackets обрабатывает входящие UDP пакеты
func (s *Server) handlePackets(ctx context.Context) error {
	s.logger.Logf("[DEBUG] handlePackets started")
	buffer := make([]byte, s.opts.BufferSize)
	readDeadline := time.Duration(s.opts.ReadDeadline) * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			s.logger.Logf("[DEBUG] handlePackets ctx.Done (main loop)")
			return nil
		case <-s.shutdownCh:
			s.logger.Logf("[DEBUG] handlePackets shutdownCh (main loop)")
			return nil
		default:
		}

		s.logger.Logf("[DEBUG] handlePackets before ReadFromUDP")

		// ✅ Обернуть ReadFromUDP в горутину с timeout
		type readPacketResult struct {
			n    int
			addr *net.UDPAddr
			err  error
		}

		readCh := make(chan readPacketResult, 1)
		go func() {
			s.conn.SetReadDeadline(time.Now().Add(readDeadline))
			n, clientAddr, err := s.conn.ReadFromUDP(buffer)
			readCh <- readPacketResult{n: n, addr: clientAddr, err: err}
		}()

		// ✅ Ждем результата или отмены контекста
		select {
		case <-ctx.Done():
			s.logger.Logf("[DEBUG] handlePackets ctx.Done (during read)")
			return nil
		case <-s.shutdownCh:
			s.logger.Logf("[DEBUG] handlePackets shutdownCh (during read)")
			return nil
		case result := <-readCh:
			s.logger.Logf("[DEBUG] handlePackets after ReadFromUDP: n=%d, err=%v", result.n, result.err)

			if result.err != nil {
				if netErr, ok := result.err.(net.Error); ok && netErr.Timeout() {
					s.logger.Logf("[DEBUG] handlePackets timeout, continuing")
					continue
				}
				s.logger.Logf("[DEBUG] handlePackets error: %v, returning", result.err)
				return nil
			}

			s.packetsProcessed.Add(1)
			s.bytesForwarded.Add(int64(result.n))

			client, err := s.getOrCreateClient(ctx, result.addr)
			if err != nil {
				s.logger.Logf("[WARN] Error getting client: %v", err)
				continue
			}

			dataCopy := make([]byte, result.n)
			copy(dataCopy, buffer[:result.n])

			select {
			case client.sendQueue <- dataCopy:
			case <-ctx.Done():
				s.logger.Logf("[DEBUG] handlePackets ctx.Done (during send)")
				return nil
			case <-s.shutdownCh:
				s.logger.Logf("[DEBUG] handlePackets shutdownCh (during send)")
				return nil
			default:
				s.packetDropped.Add(1)
			}
		}
	}
}

// getOrCreateClient получает или создает клиентское соединение
func (s *Server) getOrCreateClient(ctx context.Context, clientAddr *net.UDPAddr) (*clientConn, error) {
	clientKey := clientAddr.String()

	s.mu.RLock()
	client, exists := s.clients[clientKey]
	s.mu.RUnlock()

	if exists {
		client.lastSeen = time.Now()
		return client, nil
	}

	s.mu.RLock()
	if len(s.clients) >= s.opts.MaxConnections {
		s.mu.RUnlock()
		return nil, fmt.Errorf("max connections limit reached")
	}
	s.mu.RUnlock()

	targetAddr, err := net.ResolveUDPAddr("udp", s.targetAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve target address: %w", err)
	}

	targetConn, err := net.DialUDP("udp", nil, targetAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to target: %w", err)
	}

	if err := s.optimizeTargetSocket(targetConn); err != nil {
		s.logger.Logf("[WARN] Failed to optimize target socket: %v", err)
	}

	clientCtx, cancel := context.WithCancel(ctx)

	client = &clientConn{
		addr:       clientAddr,
		targetConn: targetConn,
		lastSeen:   time.Now(),
		cancel:     cancel,
		done:       make(chan struct{}),
		sendQueue:  make(chan []byte, 64),
	}

	s.mu.Lock()
	s.clients[clientKey] = client
	count := len(s.clients)
	s.mu.Unlock()

	s.activeClients.Add(1)
	s.logger.Logf("[DEBUG] New client: %s (total: %d)", clientAddr, count)

	client.wg.Add(2)
	go s.forwardToTargetWorker(ctx, client, clientKey)
	go s.forwardFromTarget(clientCtx, client, clientKey)

	// ✅ ДОБАВИТЬ: Горутина для закрытия соединения при отмене контекста
	go func() {
		<-clientCtx.Done()
		s.logger.Logf("[DEBUG] clientCtx.Done, closing targetConn: %s", clientKey)
		if client.targetConn != nil {
			client.targetConn.Close()
		}
	}()

	return client, nil
}

// forwardToTargetWorker отправляет пакеты на целевой сервер
func (s *Server) forwardToTargetWorker(ctx context.Context, client *clientConn, clientKey string) {
	s.logger.Logf("[DEBUG] forwardToTargetWorker started: %s", clientKey)
	defer func() {
		s.logger.Logf("[DEBUG] forwardToTargetWorker finished: %s", clientKey)
		client.wg.Done()
		s.activeClients.Add(-1)
	}()

	for {
		select {
		case <-ctx.Done():
			s.logger.Logf("[DEBUG] forwardToTargetWorker ctx.Done: %s", clientKey)
			return
		case <-client.done:
			s.logger.Logf("[DEBUG] forwardToTargetWorker client.done: %s", clientKey)
			return
		case data, ok := <-client.sendQueue:
			if !ok {
				s.logger.Logf("[DEBUG] forwardToTargetWorker sendQueue closed: %s", clientKey)
				return
			}
			if _, err := client.targetConn.Write(data); err != nil {
				s.logger.Logf("[WARN] Forward error: %v", err)
				client.cancel()
				return
			}
		}
	}
}

// forwardFromTarget получает ответы от целевого сервера
func (s *Server) forwardFromTarget(ctx context.Context, client *clientConn, clientKey string) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Logf("[ERROR] forwardFromTarget panic: %v (%s)", r, clientKey)
		}
		s.logger.Logf("[DEBUG] forwardFromTarget defer: closing resources")
		client.wg.Done()

		if client.targetConn != nil {
			client.targetConn.Close()
		}

		defer func() {
			if r := recover(); r != nil {
				s.logger.Logf("[DEBUG] Panic closing sendQueue: %v", r)
			}
		}()
		close(client.sendQueue)

		s.mu.Lock()
		delete(s.clients, clientKey)
		s.mu.Unlock()

		s.activeClients.Add(-1)

		defer func() {
			if r := recover(); r != nil {
				s.logger.Logf("[DEBUG] Panic closing done: %v", r)
			}
		}()
		close(client.done)

		s.logger.Logf("[DEBUG] forwardFromTarget finished: %s", clientKey)
	}()

	s.logger.Logf("[DEBUG] forwardFromTarget beginning loop: %s", clientKey)
	buffer := make([]byte, s.opts.BufferSize)

	for {
		// ✅ Проверка контекста
		select {
		case <-ctx.Done():
			s.logger.Logf("[DEBUG] forwardFromTarget ctx.Done: %s", clientKey)
			return
		case <-client.done:
			s.logger.Logf("[DEBUG] forwardFromTarget client.done: %s", clientKey)
			return
		default:
		}

		s.logger.Logf("[DEBUG] forwardFromTarget before Read: %s", clientKey)

		// ✅ Обернуть Read в горутину с timeout
		readCh := make(chan readResult, 1)
		go func() {
			client.targetConn.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
			n, err := client.targetConn.Read(buffer)
			readCh <- readResult{n: n, err: err}
		}()

		// ✅ Ждем результата Read или отмены контекста
		select {
		case <-ctx.Done():
			s.logger.Logf("[DEBUG] forwardFromTarget ctx.Done (during read): %s", clientKey)
			return
		case <-client.done:
			s.logger.Logf("[DEBUG] forwardFromTarget client.done (during read): %s", clientKey)
			return
		case result := <-readCh:
			s.logger.Logf("[DEBUG] forwardFromTarget after Read (n=%d, err=%v): %s", result.n, result.err, clientKey)

			if result.err != nil {
				if netErr, ok := result.err.(net.Error); ok && netErr.Timeout() {
					s.logger.Logf("[DEBUG] forwardFromTarget timeout: %s", clientKey)
					continue
				}
				s.logger.Logf("[DEBUG] forwardFromTarget read error: %v (%s)", result.err, clientKey)
				return
			}

			if s.conn == nil {
				s.logger.Logf("[DEBUG] forwardFromTarget s.conn is nil: %s", clientKey)
				return
			}

			// ✅ Проверка контекста перед write
			select {
			case <-ctx.Done():
				s.logger.Logf("[DEBUG] forwardFromTarget ctx.Done (before write): %s", clientKey)
				return
			case <-client.done:
				s.logger.Logf("[DEBUG] forwardFromTarget client.done (before write): %s", clientKey)
				return
			default:
			}

			_, err := s.conn.WriteToUDP(buffer[:result.n], client.addr)
			if err != nil {
				s.logger.Logf("[DEBUG] forwardFromTarget write error: %v (%s)", err, clientKey)
				return
			}

			client.lastSeen = time.Now()
		}
	}
}

// cleanupConnections очищает неактивные соединения
func (s *Server) cleanupConnections(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(s.opts.CleanupInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cleanupInactiveClients()
		}
	}
}

// cleanupInactiveClients удаляет неактивные соединения
func (s *Server) cleanupInactiveClients() {
	timeout := time.Duration(s.opts.InactiveTimeout) * time.Minute
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	var toDelete []string
	for key, client := range s.clients {
		if now.Sub(client.lastSeen) > timeout {
			client.cancel()
			toDelete = append(toDelete, key)
		}
	}

	if len(toDelete) > 0 {
		for _, key := range toDelete {
			delete(s.clients, key)
		}
		s.logger.Logf("[INFO] Cleaned %d inactive clients", len(toDelete))
	}
}

// statsLogger выводит статистику
func (s *Server) statsLogger(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(s.opts.StatsInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			processed := s.packetsProcessed.Load()
			forwarded := s.bytesForwarded.Load()
			active := s.activeClients.Load()
			dropped := s.packetDropped.Load()

			throughput := float64(forwarded) / time.Duration(s.opts.StatsInterval).Seconds() / 1024 / 1024
			pps := processed / int64(s.opts.StatsInterval)

			s.logger.Logf("[STATS] PPS: %d | Throughput: %.2f MB/s | Clients: %d | Dropped: %d",
				pps, throughput, active, dropped)
		}
	}
}

// closeConnection закрывает соединение
func (s *Server) closeConnection() {
	s.logger.Logf("[DEBUG] closeConnection: start")
	if s.conn != nil {
		s.logger.Logf("[DEBUG] closeConnection: closing conn")
		conn := s.conn
		s.conn = nil

		// ✅ Закрыть в фоновой горутине без ожидания
		go func() {
			s.logger.Logf("[DEBUG] closeConnection: bg goroutine closing")
			conn.Close()
			s.logger.Logf("[DEBUG] closeConnection: bg goroutine done")
		}()
	}
	s.logger.Logf("[DEBUG] closeConnection: finished (not waiting)")
}

// Stop останавливает сервер
func (s *Server) Stop() {
	s.logger.Logf("[DEBUG] Stop: entering")
	s.shutdownOnce.Do(func() {
		s.logger.Logf("[DEBUG] Stop: inside Do")

		select {
		case <-s.shutdownCh:
		default:
			close(s.shutdownCh)
		}

		s.mu.Lock()
		s.clients = make(map[string]*clientConn)
		s.mu.Unlock()

		close(s.done)
		s.logger.Logf("[DEBUG] Stop: finished")
	})
}

// WaitShutdown ждет завершения
func (s *Server) WaitShutdown(ctx context.Context) error {
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
