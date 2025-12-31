package proxy

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// MockLogger для тестирования
type MockLogger struct {
	logs []string
	mu   sync.Mutex
}

func (ml *MockLogger) Logf(format string, args ...interface{}) {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	ml.logs = append(ml.logs, fmt.Sprintf(format, args...))
}

func (ml *MockLogger) GetLogs() []string {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	return append([]string{}, ml.logs...)
}

func (ml *MockLogger) Contains(substring string) bool {
	for _, log := range ml.GetLogs() {
		if strings.Contains(log, substring) {
			return true
		}
	}
	return false
}

// Тестовый UDP сервер для целевого хоста
type TestUDPServer struct {
	conn  *net.UDPConn
	done  chan struct{}
	mu    sync.Mutex
	count int32
}

func NewTestUDPServer(port int) (*TestUDPServer, error) {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}

	server := &TestUDPServer{
		conn: conn,
		done: make(chan struct{}),
	}

	go server.handleRequests()
	return server, nil
}

func (s *TestUDPServer) handleRequests() {
	buffer := make([]byte, 65507)

	for {
		select {
		case <-s.done:
			return
		default:
		}

		s.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, clientAddr, err := s.conn.ReadFromUDP(buffer)
		if err != nil {
			continue
		}

		atomic.AddInt32(&s.count, 1)

		// Отправляем ответ обратно
		response := make([]byte, n)
		copy(response, buffer[:n])
		s.conn.WriteToUDP(response, clientAddr)
	}
}

func (s *TestUDPServer) GetCount() int32 {
	return atomic.LoadInt32(&s.count)
}

func (s *TestUDPServer) Close() error {
	close(s.done)
	return s.conn.Close()
}

// ============= UNIT TESTS =============

func TestNewServer(t *testing.T) {
	logger := &MockLogger{}
	opts := Options{
		BufferSize:       65507,
		ReadDeadline:     100,
		CleanupInterval:  30,
		InactiveTimeout:  5,
		WorkerCount:      4,
		MaxConnections:   1000,
		SocketBufferSize: 4 * 1024 * 1024,
	}

	server := NewServer("127.0.0.1", 8888, "127.0.0.1", 9999, logger, opts)

	if server == nil {
		t.Fatal("NewServer returned nil")
	}

	if server.listenPort != 8888 {
		t.Errorf("expected listenPort 8888, got %d", server.listenPort)
	}

	if server.targetPort != 9999 {
		t.Errorf("expected targetPort 9999, got %d", server.targetPort)
	}

	if server.opts.WorkerCount != 4 {
		t.Errorf("expected WorkerCount 4, got %d", server.opts.WorkerCount)
	}
}

func TestNewServerDefaultWorkers(t *testing.T) {
	logger := &MockLogger{}
	opts := Options{WorkerCount: 0}

	server := NewServer("127.0.0.1", 8888, "127.0.0.1", 9999, logger, opts)

	if server.opts.WorkerCount != 16 {
		t.Errorf("expected default WorkerCount 16, got %d", server.opts.WorkerCount)
	}
}

func TestServerStart_CannotListenOnPort(t *testing.T) {
	logger := &MockLogger{}
	opts := Options{WorkerCount: 1}

	server := NewServer("127.0.0.1", 99999, "127.0.0.1", 9999, logger, opts)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := server.Start(ctx)
	if err == nil {
		t.Fatal("expected error for invalid port")
	}
}

func TestServerStop(t *testing.T) {
	logger := &MockLogger{}
	opts := Options{
		BufferSize:       65507,
		ReadDeadline:     100,
		CleanupInterval:  30,
		InactiveTimeout:  5,
		WorkerCount:      2,
		MaxConnections:   1000,
		SocketBufferSize: 1024 * 1024,
	}

	server := NewServer("127.0.0.1", 0, "127.0.0.1", 0, logger, opts)

	// Первый Stop должен сработать
	server.Stop()

	// Второй Stop должен быть идемпотентным
	server.Stop()

	if !logger.Contains("Stopping server") {
		t.Error("expected 'Stopping server' log message")
	}
}

func TestCleanupInactiveClients(t *testing.T) {
	logger := &MockLogger{}
	opts := Options{InactiveTimeout: 1}

	server := NewServer("127.0.0.1", 8888, "127.0.0.1", 9999, logger, opts)

	// Создаем фейковых клиентов
	addr1, _ := net.ResolveUDPAddr("udp", "192.168.1.1:5000")
	addr2, _ := net.ResolveUDPAddr("udp", "192.168.1.2:5000")

	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	client1 := &clientConn{
		addr:      addr1,
		lastSeen:  time.Now().Add(-2 * time.Minute),
		cancel:    cancel,
		done:      make(chan struct{}),
		sendQueue: make(chan []byte),
	}

	client2 := &clientConn{
		addr:      addr2,
		lastSeen:  time.Now(),
		cancel:    cancel,
		done:      make(chan struct{}),
		sendQueue: make(chan []byte),
	}

	server.clients["192.168.1.1:5000"] = client1
	server.clients["192.168.1.2:5000"] = client2

	server.cleanupInactiveClients()

	if len(server.clients) != 1 {
		t.Errorf("expected 1 client after cleanup, got %d", len(server.clients))
	}

	if _, exists := server.clients["192.168.1.2:5000"]; !exists {
		t.Error("expected client2 to remain")
	}

	if _, exists := server.clients["192.168.1.1:5000"]; exists {
		t.Error("expected client1 to be removed")
	}
}

func TestMaxConnectionsLimit(t *testing.T) {
	// Запускаем реальный сервер для целевого хоста
	targetServer, err := NewTestUDPServer(19999)
	if err != nil {
		t.Fatalf("failed to start target server: %v", err)
	}
	defer targetServer.Close()

	logger := &MockLogger{}
	opts := Options{
		BufferSize:       65507,
		ReadDeadline:     100,
		CleanupInterval:  30,
		InactiveTimeout:  5,
		WorkerCount:      2,
		MaxConnections:   2, // Лимит на 2 соединения
		SocketBufferSize: 1024 * 1024,
	}

	server := NewServer("127.0.0.1", 28888, "127.0.0.1", 19999, logger, opts)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Создаем 3 клиента
	addr1, _ := net.ResolveUDPAddr("udp", "192.168.1.1:5000")
	addr2, _ := net.ResolveUDPAddr("udp", "192.168.1.2:5000")
	addr3, _ := net.ResolveUDPAddr("udp", "192.168.1.3:5000")

	// Первые два должны сработать
	_, err1 := server.getOrCreateClient(ctx, addr1)
	_, err2 := server.getOrCreateClient(ctx, addr2)
	_, err3 := server.getOrCreateClient(ctx, addr3)

	if err1 != nil {
		t.Errorf("expected first client to succeed: %v", err1)
	}

	if err2 != nil {
		t.Errorf("expected second client to succeed: %v", err2)
	}

	if err3 == nil {
		t.Error("expected third client to fail due to max connections limit")
	}

	if len(server.clients) != 2 {
		t.Errorf("expected 2 clients, got %d", len(server.clients))
	}

	// Очищаем
	for _, client := range server.clients {
		if client.targetConn != nil {
			client.targetConn.Close()
		}
	}
}

func TestOptionsValidation(t *testing.T) {
	logger := &MockLogger{}

	tests := []struct {
		name string
		opts Options
		want func(opts Options) bool
	}{
		{
			name: "default worker count",
			opts: Options{WorkerCount: 0},
			want: func(opts Options) bool { return opts.WorkerCount == 16 },
		},
		{
			name: "default max connections",
			opts: Options{MaxConnections: 0},
			want: func(opts Options) bool { return opts.MaxConnections == 10000 },
		},
		{
			name: "default buffer size",
			opts: Options{BufferSize: 0},
			want: func(opts Options) bool { return opts.BufferSize == 65507 },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewServer("127.0.0.1", 8888, "127.0.0.1", 9999, logger, tt.opts)
			if !tt.want(server.opts) {
				t.Errorf("validation failed for %s", tt.name)
			}
		})
	}
}

// ============= INTEGRATION TESTS =============

func TestEndToEndProxying(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Запускаем целевой UDP сервер
	targetServer, err := NewTestUDPServer(19999)
	if err != nil {
		t.Fatalf("failed to start target server: %v", err)
	}
	defer targetServer.Close()

	logger := &MockLogger{}
	opts := Options{
		BufferSize:       65507,
		ReadDeadline:     100,
		CleanupInterval:  30,
		InactiveTimeout:  5,
		WorkerCount:      4,
		MaxConnections:   100,
		SocketBufferSize: 1024 * 1024,
	}

	// Запускаем прокси
	server := NewServer("127.0.0.1", 28888, "127.0.0.1", 19999, logger, opts)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Запускаем сервер в отдельной горутине
	var serverErr error
	go func() {
		serverErr = server.Start(ctx)
	}()

	if serverErr != nil {
		cancel()
		t.Fatalf("failed to start server: %v", serverErr)
	}

	time.Sleep(100 * time.Millisecond) // Даем серверу время на запуск

	// Отправляем пакет через прокси
	clientAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:28888")
	if err != nil {
		cancel()
		t.Fatalf("failed to resolve client address: %v", err)
	}

	clientConn, err := net.DialUDP("udp", nil, clientAddr)
	if err != nil {
		cancel()
		t.Fatalf("failed to connect to proxy: %v", err)
	}
	defer clientConn.Close()

	testMessage := []byte("Hello, proxy!")
	_, err = clientConn.Write(testMessage)
	if err != nil {
		cancel()
		t.Fatalf("failed to send message: %v", err)
	}

	// Ждем ответа
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buffer := make([]byte, 65507)
	n, err := clientConn.Read(buffer)
	if err != nil {
		cancel()
		t.Fatalf("failed to read response: %v", err)
	}

	received := buffer[:n]
	if string(received) != string(testMessage) {
		cancel()
		t.Errorf("expected %s, got %s", testMessage, received)
	}

	cancel()
	time.Sleep(100 * time.Millisecond)
	server.WaitShutdown(context.Background())

	if logger.Contains("UDP Proxy started") {
		t.Log("Proxy started successfully")
	}
}

func TestMultipleConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	targetServer, err := NewTestUDPServer(19998)
	if err != nil {
		t.Fatalf("failed to start target server: %v", err)
	}
	defer targetServer.Close()

	logger := &MockLogger{}
	opts := Options{
		BufferSize:       65507,
		ReadDeadline:     100,
		CleanupInterval:  30,
		InactiveTimeout:  5,
		WorkerCount:      4,
		MaxConnections:   1000,
		SocketBufferSize: 1024 * 1024,
	}

	server := NewServer("127.0.0.1", 28887, "127.0.0.1", 19998, logger, opts)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	// Отправляем пакеты от разных "клиентов"
	numClients := 10
	var wg sync.WaitGroup

	for i := range numClients {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			proxyAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:28887")
			conn, err := net.DialUDP("udp", nil, proxyAddr)
			if err != nil {
				t.Logf("failed to connect: %v", err)
				return
			}
			defer conn.Close()

			msg := []byte(fmt.Sprintf("Client %d", id))
			conn.Write(msg)

			conn.SetReadDeadline(time.Now().Add(1 * time.Second))
			buffer := make([]byte, 65507)
			conn.Read(buffer)
		}(i)
	}

	wg.Wait()

	cancel()
	time.Sleep(100 * time.Millisecond)
	server.WaitShutdown(context.Background())

	if targetServer.GetCount() < int32(numClients) {
		t.Logf("expected at least %d packets, got %d", numClients, targetServer.GetCount())
	}
}

func TestGracefulShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	targetServer, err := NewTestUDPServer(19997)
	if err != nil {
		t.Fatalf("failed to start target server: %v", err)
	}
	defer targetServer.Close()

	logger := &MockLogger{}
	opts := Options{WorkerCount: 2}

	server := NewServer("127.0.0.1", 28886, "127.0.0.1", 19997, logger, opts)

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Start(ctx)
	}()

	time.Sleep(100 * time.Millisecond)

	// Отправляем пакет
	proxyAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:28886")
	conn, _ := net.DialUDP("udp", nil, proxyAddr)
	conn.Write([]byte("test"))
	conn.Close()

	time.Sleep(50 * time.Millisecond)

	// Начинаем shutdown
	cancel()
	server.Stop()

	// Ждем завершения
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	err = server.WaitShutdown(shutdownCtx)
	if err != nil {
		t.Errorf("unexpected error during shutdown: %v", err)
	}

	wg.Wait()
}

// ============= BENCHMARKS =============

func BenchmarkPacketForwarding(b *testing.B) {
	targetServer, err := NewTestUDPServer(19996)
	if err != nil {
		b.Fatalf("failed to start target server: %v", err)
	}
	defer targetServer.Close()

	logger := &MockLogger{}
	opts := Options{
		BufferSize:       65507,
		ReadDeadline:     100,
		CleanupInterval:  30,
		InactiveTimeout:  5,
		WorkerCount:      8,
		MaxConnections:   10000,
		SocketBufferSize: 4 * 1024 * 1024,
	}

	server := NewServer("127.0.0.1", 28885, "127.0.0.1", 19996, logger, opts)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	proxyAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:28885")
	conn, _ := net.DialUDP("udp", nil, proxyAddr)
	defer conn.Close()

	testData := make([]byte, 1024)

	b.ResetTimer()

	for _ = range b.N {
		conn.Write(testData)
	}

	cancel()
}

func BenchmarkServerStatistics(b *testing.B) {
	logger := &MockLogger{}
	opts := Options{WorkerCount: 4}
	server := NewServer("127.0.0.1", 8888, "127.0.0.1", 9999, logger, opts)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		server.packetsProcessed.Add(1)
		server.bytesForwarded.Add(1024)
		server.activeClients.Store(10)
	}
}
