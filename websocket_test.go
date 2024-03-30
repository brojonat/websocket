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
	ctx := context.Background()
	testBytes := []byte("testing")

	upgrader := gwebsocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024}
	upgrader.CheckOrigin = func(r *http.Request) bool { return true }

	// synchronization helpers
	doneReg := make(chan websocket.Client)
	doneUnreg := make(chan websocket.Client)

	var c websocket.Client
	manager := websocket.NewManager()
	go manager.Run(ctx)

	h := websocket.ServeWS(
		upgrader,
		websocket.DefaultSetupConn,
		websocket.NewClient,
		func(_c websocket.Client) {
			c = _c
			manager.RegisterClient(c)
			doneReg <- c
		},
		func(_c websocket.Client) {
			manager.UnregisterClient(_c)
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

	// once unregistration is done, the manager should have one client
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
	closeWait := 5 * time.Millisecond
	dl := time.Now().Add(closeWait)
	rawWS.WriteControl(gwebsocket.CloseMessage, []byte("closing"), dl)
	_p := <-doneUnreg
	is.Equal(len(manager.Clients()), 0)
	is.Equal(_p, c)
}
