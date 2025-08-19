package proxy

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

type Options struct {
	BufferSize      int `long:"buffer-size" env:"BUFFER_SIZE" default:"65507" description:"Максимальный размер UDP пакета"`
	ReadDeadline    int `long:"read-deadline" env:"READ_DEADLINE" default:"100" description:"Время ожидания ответа, мс"`
	CleanupInterval int `long:"cleanup-interval" env:"CLEANUP_INTERVAL" default:"30" description:"Интервал закрытия неактивных соединений, c"`
	InactiveTimeout int `long:"inactive-timeout" env:"INACTIVE_TIMEOUT" default:"5" description:"Время по прошествии которого клиент считается неактивным, м"`
}

// Server представляет UDP прокси сервер
type Server struct {
	opts       Options
	listenAddr string
	targetAddr string
	conn       *net.UDPConn
	clients    map[string]*clientConn
	mu         sync.RWMutex
	logger     Logger
}

// clientConn представляет соединение с клиентом
type clientConn struct {
	addr       *net.UDPAddr
	targetConn *net.UDPConn
	lastSeen   time.Time
	cancel     context.CancelFunc
}

// Logger интерфейс для логирования
type Logger interface {
	Logf(format string, args ...interface{})
}

// NewServer создает новый UDP прокси сервер
func NewServer(listenPort int, targetHost string, targetPort int, logger Logger, opts Options) *Server {
	return &Server{
		opts:       opts,
		listenAddr: fmt.Sprintf(":%d", listenPort),
		targetAddr: fmt.Sprintf("%s:%d", targetHost, targetPort),
		clients:    make(map[string]*clientConn),
		logger:     logger,
	}
}

// Start запускает UDP прокси сервер
func (s *Server) Start(ctx context.Context) error {
	// Разрешаем адрес для прослушивания
	listenAddr, err := net.ResolveUDPAddr("udp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to resolve listen address: %w", err)
	}

	// Создаем UDP соединение для прослушивания
	s.conn, err = net.ListenUDP("udp", listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on UDP: %w", err)
	}
	defer s.conn.Close()

	s.logger.Logf("[INFO] UDP Proxy server started on %s, forwarding to %s", s.listenAddr, s.targetAddr)

	// Запускаем горутину для очистки неактивных соединений
	go s.cleanupConnections(ctx)

	// Основной цикл обработки пакетов
	return s.handlePackets(ctx)
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

		// Читаем пакет от клиента
		n, clientAddr, err := s.conn.ReadFromUDP(buffer)
		if err != nil {
			s.logger.Logf("[ERROR] Error reading UDP packet: %v", err)
			continue
		}

		s.logger.Logf("[DEBUG] Received %d bytes from %s", n, clientAddr)

		// Получаем или создаем соединение с целевым сервером для этого клиента
		client, err := s.getOrCreateClient(ctx, clientAddr)
		if err != nil {
			s.logger.Logf("[ERROR] Error getting client connection: %v", err)
			continue
		}

		// Пересылаем пакет на целевой сервер
		go s.forwardToTarget(client, buffer[:n])
	}
}

// getOrCreateClient получает существующее или создает новое соединение с целевым сервером
func (s *Server) getOrCreateClient(ctx context.Context, clientAddr *net.UDPAddr) (*clientConn, error) {
	clientKey := clientAddr.String()

	s.mu.RLock()
	client, exists := s.clients[clientKey]
	s.mu.RUnlock()

	if exists {
		client.lastSeen = time.Now()
		return client, nil
	}

	// Создаем новое соединение с целевым сервером
	targetAddr, err := net.ResolveUDPAddr("udp", s.targetAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve target address: %w", err)
	}

	targetConn, err := net.DialUDP("udp", nil, targetAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to target: %w", err)
	}

	clientCtx, cancel := context.WithCancel(ctx)

	client = &clientConn{
		addr:       clientAddr,
		targetConn: targetConn,
		lastSeen:   time.Now(),
		cancel:     cancel,
	}

	s.mu.Lock()
	s.clients[clientKey] = client
	s.mu.Unlock()

	s.logger.Logf("[DEBUG] Created new connection for client %s", clientAddr)

	// Запускаем горутину для чтения ответов от целевого сервера
	go s.forwardFromTarget(clientCtx, client)

	return client, nil
}

// forwardToTarget пересылает пакет от клиента на целевой сервер
func (s *Server) forwardToTarget(client *clientConn, data []byte) {
	_, err := client.targetConn.Write(data)
	if err != nil {
		s.logger.Logf("Error forwarding to target: %v", err)
		return
	}

	s.logger.Logf("[DEBUG] Forwarded %d bytes to target from %s", len(data), client.addr)
}

// forwardFromTarget пересылает пакеты от целевого сервера обратно клиенту
func (s *Server) forwardFromTarget(ctx context.Context, client *clientConn) {
	defer client.targetConn.Close()

	buffer := make([]byte, s.opts.BufferSize)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Устанавливаем таймаут для чтения
		client.targetConn.SetReadDeadline(time.Now().Add(time.Duration(s.opts.ReadDeadline) * time.Millisecond))

		n, err := client.targetConn.Read(buffer)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			s.logger.Logf("Error reading from target: %v", err)
			return
		}

		// Отправляем ответ обратно клиенту
		_, err = s.conn.WriteToUDP(buffer[:n], client.addr)
		if err != nil {
			s.logger.Logf("Error forwarding to client: %v", err)
			return
		}

		s.logger.Logf("[DEBUG] Forwarded %d bytes from target to %s", n, client.addr)
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

// cleanupInactiveClients удаляет неактивные клиентские соединения
func (s *Server) cleanupInactiveClients() {
	timeout := time.Duration(s.opts.InactiveTimeout) * time.Minute
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	for key, client := range s.clients {
		if now.Sub(client.lastSeen) > timeout {
			s.logger.Logf("[DEBUG] Cleaning up inactive client connection: %s", client.addr)
			client.cancel()
			delete(s.clients, key)
		}
	}
}

// Stop останавливает сервер и закрывает все соединения
func (s *Server) Stop() {
	if s.conn != nil {
		s.conn.Close()
	}

	s.mu.Lock()
	for _, client := range s.clients {
		client.cancel()
	}
	s.clients = make(map[string]*clientConn)
	s.mu.Unlock()

	s.logger.Logf("[INFO] UDP Proxy server stopped")
}
