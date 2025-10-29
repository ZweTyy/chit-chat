package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	chitchat "github.com/ZweTyy/chit-chat/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type lamport struct{ t int64 }

func (l *lamport) tickLocal() int64 {
	l.t++
	return l.t
}
func (l *lamport) onRecv(remote int64) int64 {
	if remote > l.t {
		l.t = remote
	}
	l.t++
	return l.t
}

func main() {
	var (
		addr = flag.String("addr", "localhost:50051", "server address")
		id   = flag.String("id", "", "client id (required)")
		name = flag.String("name", "", "display name")
	)
	flag.Parse()
	if strings.TrimSpace(*id) == "" {
		log.Fatal("missing -id")
	}

	log.Printf("Starting client %s...", *id)

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDial()

	conn, err := grpc.DialContext(
		dialCtx,
		*addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		log.Fatalf("Dial error: %v", err)
	}
	defer conn.Close()

	client := chitchat.NewChitChatClient(conn)

	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()

	stream, err := client.Chat(streamCtx)
	if err != nil {
		log.Fatalf("Stream open error: %v", err)
	}

	var L lamport

	// send join
	L.tickLocal()
	if err := stream.Send(&chitchat.ClientEvent{
		Type:        chitchat.ClientEventType_CLIENT_EVENT_TYPE_JOIN,
		ClientId:    *id,
		DisplayName: *name,
		LogicalTime: &chitchat.LogicalTime{T: L.t},
	}); err != nil {
		log.Fatalf("Send JOIN error: %v", err)
	}

	go func() {
		for {
			ev, err := stream.Recv()
			if err != nil {
				log.Printf("Connection closed: %v", err)
				os.Exit(0)
			}

			prev := L.t
			srvT := ev.GetLogicalTime().GetT()
			L.onRecv(srvT)

			var pretty string
			switch ev.GetType() {
			case chitchat.ServerEventType_SERVER_EVENT_TYPE_CHAT:
				pretty = fmt.Sprintf("<%s>: %s", ev.GetClientId(), ev.GetContent())
			case chitchat.ServerEventType_SERVER_EVENT_TYPE_SYSTEM:
				c := ev.GetContent()
				if strings.Contains(c, "joined") {
					pretty = fmt.Sprintf("%s has joined the chat", ev.GetClientId())
				} else if strings.Contains(c, "left") {
					pretty = fmt.Sprintf("%s has left the chat", ev.GetClientId())
				} else {
					pretty = c
				}
			default:
				pretty = ev.GetContent()
			}

			log.Printf("[Prev: %d, Time: %d] Server: %s", prev, srvT, pretty)
		}
	}()

	sc := bufio.NewScanner(os.Stdin)
	fmt.Println("Type messages (max 128 chars). Type '/leave' to exit.")
	for sc.Scan() {
		txt := strings.TrimSpace(sc.Text())
		if txt == "" {
			continue
		}
		if txt == "/leave" {
			L.tickLocal()
			_ = stream.Send(&chitchat.ClientEvent{
				Type:        chitchat.ClientEventType_CLIENT_EVENT_TYPE_LEAVE,
				ClientId:    *id,
				LogicalTime: &chitchat.LogicalTime{T: L.t},
			})
			log.Printf("You left the chat.")
			return
		}
		// 128 chars
		rs := []rune(txt)
		if len(rs) > 128 {
			rs = rs[:128]
		}
		L.tickLocal()
		if err := stream.Send(&chitchat.ClientEvent{
			Type:        chitchat.ClientEventType_CLIENT_EVENT_TYPE_CHAT,
			ClientId:    *id,
			Content:     string(rs),
			LogicalTime: &chitchat.LogicalTime{T: L.t},
		}); err != nil {
			log.Printf("Send chat error: %v", err)
			return
		}
	}
	if err := sc.Err(); err != nil {
		log.Printf("Stdin error: %v", err)
	}
}
