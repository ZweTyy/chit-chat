package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	chitchat "github.com/ZweTyy/chit-chat/grpc"
	"google.golang.org/grpc"
)

type server struct {
	chitchat.UnimplementedChitChatServer

	mu      sync.Mutex
	clients map[string]*clientConn
	Ls      int64
}

type clientConn struct {
	id    string
	name  string
	send  chan *chitchat.ServerEvent
	done  chan struct{}
	alive bool
}

func newServer() *server {
	return &server{
		clients: make(map[string]*clientConn),
		Ls:      0,
	}
}

func (s *server) tickFrom(remote int64) int64 {
	if remote > s.Ls {
		s.Ls = remote
	}
	s.Ls++
	return s.Ls
}

func (s *server) broadcast(ev *chitchat.ServerEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clients {
		select {
		case c.send <- ev:
		default:
		}
	}
}

func (s *server) addClient(id, name string) (*clientConn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.clients[id]; exists {
		return nil, fmt.Errorf("client id already in use: %s", id)
	}
	cc := &clientConn{
		id:    id,
		name:  name,
		send:  make(chan *chitchat.ServerEvent, 64),
		done:  make(chan struct{}),
		alive: true,
	}
	s.clients[id] = cc
	return cc, nil
}

func (s *server) removeClient(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.clients[id]; ok {
		if c.alive {
			c.alive = false
			close(c.done)
			close(c.send)
		}
		delete(s.clients, id)
	}
}

func (s *server) Chat(stream chitchat.ChitChat_ChatServer) error {
	var (
		clientID   string
		clientName string
		cc         *clientConn
	)

	sendLoop := func() error {
		for {
			select {
			case ev, ok := <-cc.send:
				if !ok {
					return nil
				}
				if err := stream.Send(ev); err != nil {
					return err
				}
			case <-cc.done:
				return nil
			case <-stream.Context().Done():
				return stream.Context().Err()
			}
		}
	}

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetType() != chitchat.ClientEventType_CLIENT_EVENT_TYPE_JOIN {
		return errors.New("first client event must be JOIN")
	}
	clientID = strings.TrimSpace(first.GetClientId())
	if clientID == "" {
		return errors.New("JOIN requires non-empty client_id")
	}
	clientName = strings.TrimSpace(first.GetDisplayName())

	// register client
	cc, err = s.addClient(clientID, clientName)
	if err != nil {
		return err
	}

	go func() {
		if err := sendLoop(); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("send loop error for %s: %v", clientID, err)
		}
	}()

	L := s.tickFrom(first.GetLogicalTime().GetT())
	joinMsg := fmt.Sprintf("Participant %s joined Chit Chat at logical time %d", clientID, L)
	s.broadcast(&chitchat.ServerEvent{
		Type:        chitchat.ServerEventType_SERVER_EVENT_TYPE_SYSTEM,
		ClientId:    clientID,
		DisplayName: clientName,
		Content:     joinMsg,
		LogicalTime: &chitchat.LogicalTime{T: L},
	})
	log.Printf("[Time: %d] %s: %s has joined the chat", L, clientID, clientID)

	for {
		ev, err := stream.Recv()
		if err != nil {
			break
		}
		switch ev.GetType() {
		case chitchat.ClientEventType_CLIENT_EVENT_TYPE_CHAT:
			msg := ev.GetContent()
			if len([]rune(msg)) > 128 {
				msg = string([]rune(msg)[:128])
			}
			L := s.tickFrom(ev.GetLogicalTime().GetT())
			s.broadcast(&chitchat.ServerEvent{
				Type:        chitchat.ServerEventType_SERVER_EVENT_TYPE_CHAT,
				ClientId:    clientID,
				DisplayName: clientName,
				Content:     msg,
				LogicalTime: &chitchat.LogicalTime{T: L},
			})
			log.Printf("[Time: %d] %s: <%s>: %s", L, clientID, clientID, msg)

		case chitchat.ClientEventType_CLIENT_EVENT_TYPE_LEAVE:
			L := s.tickFrom(ev.GetLogicalTime().GetT())
			leaveMsg := fmt.Sprintf("Participant %s left Chit Chat at logical time %d", clientID, L)
			s.broadcast(&chitchat.ServerEvent{
				Type:        chitchat.ServerEventType_SERVER_EVENT_TYPE_SYSTEM,
				ClientId:    clientID,
				DisplayName: clientName,
				Content:     leaveMsg,
				LogicalTime: &chitchat.LogicalTime{T: L},
			})
			log.Printf("[Time: %d] %s: %s has left the chat", L, clientID, clientID)

			s.removeClient(clientID)
			return nil
		}
	}

	L = s.tickFrom(0)
	leaveMsg := fmt.Sprintf("Participant %s left Chit Chat at logical time %d", clientID, L)
	s.broadcast(&chitchat.ServerEvent{
		Type:        chitchat.ServerEventType_SERVER_EVENT_TYPE_SYSTEM,
		ClientId:    clientID,
		DisplayName: clientName,
		Content:     leaveMsg,
		LogicalTime: &chitchat.LogicalTime{T: L},
	})
	log.Printf("[Time: %d] %s: %s has left the chat", L, clientID, clientID)

	s.removeClient(clientID)
	return nil
}

func main() {
	addr := ":50051"

	log.Printf("Starting Server...")
	log.Printf("Listening on port: %s", strings.TrimPrefix(addr, ":"))

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen error: %v", err)
	}

	grpcServer := grpc.NewServer()
	chitchat.RegisterChitChatServer(grpcServer, newServer())

	// shutdown
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		log.Printf("Shutting down...")
		grpcServer.GracefulStop()
	}()

	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("serve error: %v", err)
	}
}
