package websocket_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brojonat/websocket"
	gwebsocket "github.com/gorilla/websocket"
	"github.com/matryer/is"
)

func TestWSHandler(t *testing.T) {
	is := is.New(t)
	testBytes := []byte("testing")

	upgrader := gwebsocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024}
	upgrader.CheckOrigin = func(r *http.Request) bool { return true }

	// synchronization helpers
	doneReg := make(chan websocket.Client)
	doneUnreg := make(chan websocket.Client)

	var c websocket.Client
	var cf context.CancelFunc

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	manager := websocket.NewManager()
	go manager.Run(ctx)

	h := websocket.ServeWS(
		upgrader,
		websocket.DefaultSetupConn,
		websocket.NewClient,
		func(ctx context.Context, _cf context.CancelFunc, _c websocket.Client) {
			cf = _cf
			c = _c
			manager.RegisterClient(ctx, cf, c)
			doneReg <- c
		},
		func(_c websocket.Client) {
			manager.UnregisterClient(_c)
			_c.Wait()
			doneUnreg <- _c
		},
		50*time.Second,
		[]func([]byte){func(b []byte) { c.Write(b) }},
	)

	// setup and connect to the the test server using a basic websocket
	s := httptest.NewServer(h)

	defer s.Close()
	rawWS, _, err := gwebsocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(s.URL, "http"), nil)
	is.NoErr(err)
	defer rawWS.Close()

	// once registration is done, the manager should have one client
	<-doneReg
	is.Equal(len(manager.Clients()), 1)

	// write a message to the server; this will be echoed back
	err = rawWS.WriteMessage(gwebsocket.TextMessage, testBytes)
	is.NoErr(err)
	_, msg, err := rawWS.ReadMessage()
	is.NoErr(err)
	is.Equal(msg, testBytes)

	// close the connection, which should trigger the server to cleanup and
	// unregister the client connection
	rawWS.WriteControl(gwebsocket.CloseMessage, nil, time.Now().Add(1*time.Second))
	_p := <-doneUnreg
	is.Equal(len(manager.Clients()), 0)
	is.Equal(_p, c)
	time.Sleep(1 * time.Second)
	//FIXME: seems to be leaking goroutines
}
