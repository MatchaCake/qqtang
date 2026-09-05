package probe

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"qqtang/internal/protocol/game"
)

type shutdownLogWriter struct {
	closed atomic.Bool
	late   chan struct{}
}

func (writer *shutdownLogWriter) Write(data []byte) (int, error) {
	if writer.closed.Load() {
		select {
		case writer.late <- struct{}{}:
		default:
		}
	}
	return len(data), nil
}

func TestShutdownCancelsPendingInventoryRefresh(t *testing.T) {
	server, err := New(Config{CaptureRoot: t.TempDir(), MaxPacketSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	writer := &shutdownLogWriter{late: make(chan struct{}, 1)}
	server.logWriter = writer
	server.schedulePurchasedItemRefresh(1000001, 22, game.NewPermanentItemInfo(22, 1))
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	writer.closed.Store(true)
	select {
	case <-writer.late:
		t.Fatal("delayed inventory callback wrote logs after Close released its resources")
	case <-time.After(250 * time.Millisecond):
	}
}

type delayedAcceptedConnection struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
}

type shutdownScanSignal struct{ scanned chan struct{} }

func (signal shutdownScanSignal) Close() error { close(signal.scanned); return nil }

func (connection *delayedAcceptedConnection) LocalAddr() net.Addr {
	close(connection.entered)
	<-connection.release
	return connection.Conn.LocalAddr()
}

func TestShutdownClosesAcceptedConnectionBeforeLateRegistration(t *testing.T) {
	server, err := New(Config{CaptureRoot: t.TempDir(), MaxPacketSize: 64, IdleTimeoutMS: 30000})
	if err != nil {
		t.Fatal(err)
	}
	server.logWriter = io.Discard
	scanned := make(chan struct{})
	server.competitiveAICloser = shutdownScanSignal{scanned}
	serverEnd, clientEnd := net.Pipe()
	defer clientEnd.Close()
	defer serverEnd.Close()
	connection := &delayedAcceptedConnection{Conn: serverEnd, entered: make(chan struct{}), release: make(chan struct{})}
	server.wg.Go(func() { server.handleTCP(context.Background(), connection, ListenerConfig{}, "late-accepted") })
	<-connection.entered
	closed := make(chan error, 1)
	go func() { closed <- server.Close() }()
	// The backend closes after the accepted-connection scan and before wg.Wait.
	<-scanned
	close(connection.release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		_ = clientEnd.Close()
		<-closed
		t.Fatal("shutdown missed the accepted socket and waited for its idle deadline")
	}
}
