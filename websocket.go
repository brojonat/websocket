package websocket

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// DefaultSetupClient is an example implementation of a function that sets up a
// websocket connection.
func DefaultSetupConn(c *websocket.Conn) {
	pw := 60 * time.Second
	c.SetReadLimit(512)
	c.SetReadDeadline(time.Now().Add(pw))
	c.SetPongHandler(func(string) error {
		c.SetReadDeadline(time.Now().Add(pw))
		return nil
	})
}

// Client is an interface for reading from and writing to a websocket
// connection. It is designed to be used as a middleman between a service and a
// client websocket connection.
type Client interface {
	io.Writer
	io.Closer

	// WritePump is responsible for writing messages to the client (including
	// the regularly spaced ping messages)
	WritePump(func(Client), time.Duration)

	// ReadPump is responsible for reading messages from the client, and passing
	// them to the message handlers
	ReadPump(func(Client), ...func([]byte))

	// Log allows consumers to inject their own logging dependencies
	Log(int, string, ...any)
}

// ServeWS upgrades HTTP connections to WebSocket, creates the Client, calls the
// onCreate callback, and starts goroutines that handle reading (writing)
// from (to) the client.
func ServeWS(
	// upgrader upgrades the connection
	upgrader websocket.Upgrader,
	// connSetup is called on the upgraded WebSocket connection to configure
	// the connection
	connSetup func(*websocket.Conn),
	// clientFactory is a function that takes a connection and returns a new Client
	clientFactory func(*websocket.Conn) Client,
	// onCreate is a function to call once the Client is created (e.g.,
	// store it in a some collection on the service for later reference)
	onCreate func(Client),
	// onDestroy is a function to call after the WebSocket connection is closed
	// (e.g., remove it from the collection on the service)
	onDestroy func(Client),
	// ping is the interval at which ping messages are aren't sent
	ping time.Duration,
	// msgHandlers are callbacks that handle messages received from the client
	msgHandlers []func([]byte),
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			// if Upgrade fails it closes the connection, so just return
			return
		}
		connSetup(conn)
		client := clientFactory(conn)
		onCreate(client)

		// all writes will happen in this goroutine, ensuring only one write on
		// the connection at a time
		go client.WritePump(onDestroy, ping)

		// all reads will happen in this goroutine, ensuring only one reader on
		// the connection at a time
		go client.ReadPump(onDestroy, msgHandlers...)
	}
}

type client struct {
	conn   *websocket.Conn
	egress chan []byte
	logger *slog.Logger
}

// NewClient returns a new Client from a *websocket.Conn. This can be passed to
// ServeWS as the client factory arg.
func NewClient(c *websocket.Conn) Client {
	return &client{
		conn:   c,
		egress: make(chan []byte, 32),
		logger: slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}
}

// Write implements the Writer interface.
func (c *client) Write(p []byte) (int, error) {
	c.egress <- p
	return len(p), nil
}

// Close implements the Closer interface. Note the behavior of calling Close()
// multiple times is undefined; this implementation swallows all errors.
func (c *client) Close() error {
	c.conn.WriteControl(websocket.CloseMessage, []byte{}, time.Time{})
	c.conn.Close()
	return nil
}

// WritePump serially processes messages from the egress channel and writes them
// to the client, ensuring that all writes to the underlying connection are
// performed here.
func (c *client) WritePump(onDestroy func(Client), ping time.Duration) {

	// create a ticker that triggers a ping at given interval
	pingTicker := time.NewTicker(ping)
	defer func() {
		pingTicker.Stop()
		onDestroy(c)
	}()

	for {
		select {
		case msgBytes, ok := <-c.egress:
			// ok will be false in case the egress channel is closed
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			// write a message to the connection
			if err := c.conn.WriteMessage(websocket.TextMessage, msgBytes); err != nil {
				c.Log(int(slog.LevelError), fmt.Sprintf("error writing message: %v", err))
				return
			}
		case <-pingTicker.C:
			if err := c.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				c.Log(int(slog.LevelError), fmt.Sprintf("error writing ping: %v", err))
				return
			}
		}
	}
}

// ReadPump serially processes messages from the client and passes them to the
// supplied message handlers in their own goroutine. Each message will be processed
// serially, but the handlers are executed concurrently.
func (c *client) ReadPump(
	unregisterFunc func(Client),
	handlers ...func([]byte),
) {
	// unregister and close before exit
	defer func() {
		unregisterFunc(c)
	}()

	// read forever
	for {
		_, payload, err := c.conn.ReadMessage()

		if err != nil {
			// client may have simply closed the connection
			if !websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway) {
				c.Log(int(slog.LevelDebug), "client closed connection")
				c.Close()
				break
			}
			// unexpected error, close the connection
			c.Log(int(slog.LevelError), fmt.Sprintf("error reading message: %v", err))
			c.Close()
			break
		}

		// handle the message
		var wg sync.WaitGroup
		wg.Add(len(handlers))
		for _, h := range handlers {
			go func(h func([]byte)) {
				h(payload)
				wg.Add(1)
			}(h)
		}
		wg.Done()
	}
}

func (c *client) Log(level int, s string, args ...any) {
	switch level {
	case int(slog.LevelDebug):
		c.logger.Debug(s, args...)
	case int(slog.LevelInfo):
		c.logger.Info(s, args...)
	case int(slog.LevelWarn):
		c.logger.Warn(s, args...)
	case int(slog.LevelError):
		c.logger.Error(s, args...)
	}
}

// Manager maintains a set of Clients.
type Manager interface {
	Clients() []Client
	RegisterClient(Client)
	UnregisterClient(Client)
	Run(context.Context)
}

type manager struct {
	mu         sync.RWMutex
	clients    map[Client]struct{}
	register   chan regreq
	unregister chan regreq
}

type regreq struct {
	wp   Client
	done chan struct{}
}

func NewManager() Manager {
	return &manager{
		mu:         sync.RWMutex{},
		clients:    make(map[Client]struct{}),
		register:   make(chan regreq),
		unregister: make(chan regreq),
	}
}

// Clients returns the currently managed Clients.
func (m *manager) Clients() []Client {
	res := []Client{}
	m.mu.RLock()
	defer m.mu.RUnlock()

	for c := range m.clients {
		res = append(res, c)
	}
	return res
}

// RegisterClient adds the Client to the Manager's store.
func (m *manager) RegisterClient(wp Client) {
	done := make(chan struct{})
	rr := regreq{
		wp:   wp,
		done: done,
	}
	m.register <- rr
	<-done
}

// UnregisterClient removes the Client from the Manager's store.
func (m *manager) UnregisterClient(wp Client) {
	done := make(chan struct{})
	rr := regreq{
		wp:   wp,
		done: done,
	}
	m.unregister <- rr
	<-done
}

// Run runs in its own goroutine processing (un)registration requests.
func (m *manager) Run(ctx context.Context) {
	// helper fn for cleaning up client
	cleanupClient := func(c Client) {
		// delete from map
		delete(m.clients, c)
		// close connections
		c.Close()
	}

	for {
		select {
		case rr := <-m.register:
			m.mu.Lock()
			m.clients[rr.wp] = struct{}{}
			m.mu.Unlock()
			rr.done <- struct{}{}

		case rr := <-m.unregister:
			m.mu.Lock()
			if _, ok := m.clients[rr.wp]; ok {
				cleanupClient(rr.wp)
			}
			m.mu.Unlock()
			rr.done <- struct{}{}

		case <-ctx.Done():
			m.mu.Lock()
			for client := range m.clients {
				cleanupClient(client)
			}
			m.mu.Unlock()
		}
	}
}

// Broadcaster is an example implementation of Manager that has a
// Broadcast method that writes the supplied message to all clients.
type Broadcaster struct {
	*manager
}

func NewBroadcaster() Manager {
	m := manager{
		mu:         sync.RWMutex{},
		clients:    make(map[Client]struct{}),
		register:   make(chan regreq),
		unregister: make(chan regreq),
	}
	return &Broadcaster{
		manager: &m,
	}
}

func (bb *Broadcaster) Broadcast(b []byte) {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	for w := range bb.clients {
		w.Write(b)
	}

}
