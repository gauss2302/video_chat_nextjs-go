package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Message types
const (
	MessageTypeOffer     = "offer"
	MessageTypeAnswer    = "answer"
	MessageTypeCandidate = "candidate"
)

// Room represents a video chat room
type Room struct {
	clients    map[string]*Client
	broadcast  chan Message
	register   chan *Client
	unregister chan *Client
	mutex      sync.RWMutex
}

// Client represents a connected client
type Client struct {
	ID   string
	Conn *websocket.Conn
	Room *Room
}

// Message represents a WebRTC signaling message
type Message struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
	From string          `json:"from,omitempty"`
	To   string          `json:"to,omitempty"`
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // In production, implement proper origin checking
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func newRoom() *Room {
	return &Room{
		clients:    make(map[string]*Client),
		broadcast:  make(chan Message),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

func (r *Room) run() {
	for {
		select {
		case client := <-r.register:
			r.mutex.Lock()
			r.clients[client.ID] = client
			r.mutex.Unlock()

			// Notify other clients about new user
			r.broadcastUserState(client.ID, true)

		case client := <-r.unregister:
			r.mutex.Lock()
			if _, ok := r.clients[client.ID]; ok {
				delete(r.clients, client.ID)
				client.Conn.Close()
				// Notify other clients about user leaving
				r.broadcastUserState(client.ID, false)
			}
			r.mutex.Unlock()

		case message := <-r.broadcast:
			r.handleMessage(message)
		}
	}
}

func (r *Room) handleMessage(message Message) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	switch message.Type {
	case MessageTypeOffer, MessageTypeAnswer, MessageTypeCandidate:
		// If "to" is specified, send only to that client
		if message.To != "" {
			if client, ok := r.clients[message.To]; ok {
				err := client.Conn.WriteJSON(message)
				if err != nil {
					log.Printf("error sending to client %s: %v", message.To, err)
				}
			}
		} else {
			// Otherwise broadcast to all other clients
			for id, client := range r.clients {
				if id != message.From {
					err := client.Conn.WriteJSON(message)
					if err != nil {
						log.Printf("error broadcasting to client %s: %v", id, err)
					}
				}
			}
		}
	}
}

func (r *Room) broadcastUserState(userID string, joined bool) {
	messageType := "user-joined"
	if !joined {
		messageType = "user-left"
	}

	message := Message{
		Type: messageType,
		Data: json.RawMessage(`{"userId":"` + userID + `"}`),
	}

	for id, client := range r.clients {
		if id != userID {
			err := client.Conn.WriteJSON(message)
			if err != nil {
				log.Printf("error broadcasting user state: %v", err)
			}
		}
	}
}

func (r *Room) handleWebSocket(w http.ResponseWriter, req *http.Request) {
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		log.Printf("upgrade error: %v", err)
		return
	}

	// Create new client with unique ID
	client := &Client{
		ID:   generateID(),
		Conn: conn,
		Room: r,
	}

	// Set read/write deadlines
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))

	// Configure ping/pong handlers
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	r.register <- client

	// Start ping ticker
	go func() {
		ticker := time.NewTicker(54 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second)); err != nil {
					return
				}
			}
		}
	}()

	defer func() {
		r.unregister <- client
	}()

	// Send client ID
	initMessage := Message{
		Type: "init",
		Data: json.RawMessage(`{"clientId":"` + client.ID + `"}`),
	}
	err = conn.WriteJSON(initMessage)
	if err != nil {
		log.Printf("error sending init message: %v", err)
		return
	}

	for {
		var message Message
		err := conn.ReadJSON(&message)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("error: %v", err)
			}
			break
		}

		// Add sender ID to message
		message.From = client.ID
		r.broadcast <- message
	}
}

// generateID generates a simple unique ID
func generateID() string {
	return time.Now().Format("20060102150405.000000")
}

func main() {
	// Get port from environment or use default
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Enable logging with timestamps
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	// Create and start room
	room := newRoom()
	go room.run()

	// Configure CORS
	corsMiddleware := func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}

			handler.ServeHTTP(w, r)
		})
	}

	// Setup routes
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", room.handleWebSocket)
	mux.Handle("/", http.FileServer(http.Dir("frontend")))

	// Apply CORS middleware
	handler := corsMiddleware(mux)

	// Start server
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 3 * time.Second,
	}

	log.Printf("Server starting on :%s", port)
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
