package anytls

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"

	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/anytls/sing-anytls/padding"
)

type testConnectionHandler struct{}

func (testConnectionHandler) NewConnectionEx(context.Context, net.Conn, M.Socksaddr, M.Socksaddr, N.CloseHandlerFunc) {
}

func TestServiceUserUpdatesAreRaceFree(t *testing.T) {
	service, err := NewService(ServiceConfig{
		PaddingScheme: padding.DefaultPaddingScheme,
		Users:         []User{{Name: "1", Password: "initial"}},
		Handler:       testConnectionHandler{},
		Logger:        logger.NOP(),
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			service.UpdateUsers([]User{{Name: "1", Password: "password"}, {Name: "2", Password: "password-2"}})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 250; i++ {
			server, client := net.Pipe()
			go func() {
				defer client.Close()
				_, _ = client.Write(bytes.Repeat([]byte{0xA5}, 32))
			}()
			_ = service.NewConnection(context.Background(), server, M.Socksaddr{}, nil)
			_ = server.Close()
		}
	}()
	wg.Wait()
}
