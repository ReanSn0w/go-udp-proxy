package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const SOL_UDP = 17 // Linux

type Options struct {
	BufferSize       int  `long:"buffer-size" env:"BUFFER_SIZE" default:"65507" description:"Максимальный размер UDP пакета"`
	ReadDeadline     int  `long:"read-deadline" env:"READ_DEADLINE" default:"100" description:"Время ожидания ответа, мс"`
	CleanupInterval  int  `long:"cleanup-interval" env:"CLEANUP_INTERVAL" default:"30" description:"Интервал закрытия неактивных соединений, c"`
	InactiveTimeout  int  `long:"inactive-timeout" env:"INACTIVE_TIMEOUT" default:"5" description:"Время по прошествии которого клиент считается неактивным, м"`
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
	}
}

// Start запускает UDP прокси сервер
func (s *Server) Start(ctx context.Context) error {
	listenAddr, err := net.ResolveUDPAddr("udp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to resolve listen address: %w", err)
	}

	s.conn, err = net.ListenUDP("udp", listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on UDP: %w", err)
	}
	defer s.closeConnection()

	if err := s.optimizeListenSocket(); err != nil {
		s.logger.Logf("[WARN] Failed to optimize listen socket: %v", err)
	}

	s.logger.Logf("[INFO] UDP Proxy started on %s -> %s", s.listenAddr, s.targetAddr)
	s.logger.Logf("[INFO] Workers: %d | Buffer: %d bytes | Socket buffer: %d bytes",
		s.opts.WorkerCount, s.opts.BufferSize, s.opts.SocketBufferSize)

	go s.cleanupConnections(ctx)
	go s.statsLogger(ctx)

	go func() {
		<-ctx.Done()
		if s.conn != nil {
			s.conn.Close()
		}
	}()

	return s.handlePackets(ctx)
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
	buffer := make([]byte, s.opts.BufferSize)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, clientAddr, err := s.conn.ReadFromUDP(buffer)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-ctx.Done():
				return nil
			default:
				s.logger.Logf("[ERROR] Error reading UDP packet: %v", err)
				return err
			}
		}

		s.packetsProcessed.Add(1)
		s.bytesForwarded.Add(int64(n))

		client, err := s.getOrCreateClient(ctx, clientAddr)
		if err != nil {
			s.logger.Logf("[WARN] Error getting client: %v", err)
			continue
		}

		dataCopy := make([]byte, n)
		copy(dataCopy, buffer[:n])

		select {
		case client.sendQueue <- dataCopy:
		case <-ctx.Done():
			return ctx.Err()
		default:
			s.packetDropped.Add(1)
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

	s.activeClients.Store(int32(count))
	s.logger.Logf("[DEBUG] New client: %s (total: %d)", clientAddr, count)

	client.wg.Add(2)
	go s.forwardToTargetWorker(ctx, client, clientKey)
	go s.forwardFromTarget(clientCtx, client, clientKey)

	return client, nil
}

// forwardToTargetWorker отправляет пакеты на целевой сервер
func (s *Server) forwardToTargetWorker(ctx context.Context, client *clientConn, clientKey string) {
	defer client.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case <-client.done:
			return
		case data, ok := <-client.sendQueue:
			if !ok {
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
		client.wg.Done()
		client.targetConn.Close()
		close(client.sendQueue)

		s.mu.Lock()
		delete(s.clients, clientKey)
		count := len(s.clients)
		s.mu.Unlock()

		s.activeClients.Store(int32(count))
		close(client.done)
	}()

	buffer := make([]byte, s.opts.BufferSize)
	readDeadline := time.Duration(s.opts.ReadDeadline) * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			return
		case <-client.done:
			return
		default:
		}

		client.targetConn.SetReadDeadline(time.Now().Add(readDeadline))
		n, err := client.targetConn.Read(buffer)

		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			if err != io.EOF {
				s.logger.Logf("[DEBUG] Read error: %v", err)
			}
			return
		}

		_, err = s.conn.WriteToUDP(buffer[:n], client.addr)
		if err != nil {
			s.logger.Logf("[WARN] Write error: %v", err)
			return
		}

		client.lastSeen = time.Now()
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
	ticker := time.NewTicker(30 * time.Second)
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

			throughput := float64(forwarded) / 30.0 / 1024 / 1024
			pps := processed / 30

			s.logger.Logf("[STATS] PPS: %d | Throughput: %.2f MB/s | Clients: %d | Dropped: %d",
				pps, throughput, active, dropped)
		}
	}
}

// closeConnection закрывает соединение
func (s *Server) closeConnection() {
	if s.conn != nil {
		if err := s.conn.Close(); err != nil {
			s.logger.Logf("[WARN] Close error: %v", err)
		}
	}
}

// Stop останавливает сервер
func (s *Server) Stop() {
	s.shutdownOnce.Do(func() {
		s.logger.Logf("[INFO] Stopping server...")

		s.closeConnection()

		s.mu.Lock()
		clients := make([]*clientConn, 0, len(s.clients))
		for _, client := range s.clients {
			clients = append(clients, client)
		}
		s.clients = make(map[string]*clientConn)
		s.mu.Unlock()

		for _, client := range clients {
			client.cancel()
		}

		for _, client := range clients {
			client.wg.Wait()
		}

		close(s.done)
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
