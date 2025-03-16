package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
    "os"
	"github.com/mr-tron/base58"
	"github.com/gorilla/websocket"
    "github.com/joho/godotenv"
)

const solanaWebSocketURL = "wss://mainnet.helius-rpc.com/?api-key="

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

var clientStatus = ClientStatus{
	clients: make(map[*websocket.Conn]bool),
}

type ClientStatus struct {
	clients map[*websocket.Conn]bool
	mu      sync.Mutex
}

var HELIUS_KEY string

type Result struct {
	Context struct {
		Slot int64 `json:"slot"`
	} `json:"context"`
	Params struct {
		Result struct {
			Value struct {
				Signature string   `json:"signature"`
				Err       *string  `json:"err"` // Err can be null, so we use a pointer
				Logs      []string `json:"logs"`
			} `json:"value"`
		} `json:"result"`
	} `json:"params"`
}

var messageChannel = make(chan string)

func handleClientWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Error upgrading connection:", err)
		return
	}
	defer conn.Close()

	clientStatus.mu.Lock()
	clientStatus.clients[conn] = true
	clientStatus.mu.Unlock()

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			log.Println("Client disconnected:", err)
			break
		}
	}

	clientStatus.mu.Lock()
	delete(clientStatus.clients, conn)
	clientStatus.mu.Unlock()
}

func sendToClients() {
	for message := range messageChannel {
		clientStatus.mu.Lock()
		for client := range clientStatus.clients {
			err := client.WriteMessage(websocket.TextMessage, []byte(message))
			if err != nil {
				log.Println("Error sending message to client:", err)
				client.Close()
				delete(clientStatus.clients, client)
			}
		}
		clientStatus.mu.Unlock()
	}
}

func createSolanaWSConnection() (*websocket.Conn, error) {
	conn, r, err := websocket.DefaultDialer.Dial(solanaWebSocketURL + string(HELIUS_KEY), nil)
	if err != nil {
		return nil, err
	}
	log.Println("Connected to Solana WebSocket. Status:", r.StatusCode)

	conn.SetPingHandler(func(appData string) error {
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})

	return conn, nil
}

func subscribeToSolana(conn *websocket.Conn) {
	subscriptionMessage := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "logsSubscribe",
		"params": []interface{}{
			map[string]interface{}{
				"mentions": []string{"11111111111111111111111111111111"},
			},
			map[string]interface{}{
				"commitment": "finalized",
				"encoding":   "jsonParsed",
			},
		},
	}

	subscriptionMessageJSON, err := json.Marshal(subscriptionMessage)
	if err != nil {
		log.Println("Error marshalling subscription message:", err)
		return
	}

	if err := conn.WriteMessage(websocket.TextMessage, subscriptionMessageJSON); err != nil {
		log.Println("Error subscribing to Solana WebSocket:", err)
	}
}

func listenForSolanaEvents() {
	for {
		conn, err := createSolanaWSConnection()
		if err != nil {
			log.Println("Error connecting to Solana WebSocket:", err)
			time.Sleep(5 * time.Second)
			continue
		}
		subscribeToSolana(conn)

		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				log.Println("Error reading from Solana WebSocket:", err)
				break
			}

			var messageLogs Result
			if err = json.Unmarshal(msg, &messageLogs); err != nil {
				log.Println("Not expected data, skipping.")
				continue
			}
			decodeProgramData(messageLogs.Params.Result.Value.Logs)
			messageChannel <- string(msg)
		}

		conn.Close()
		log.Println("Disconnected from Solana WebSocket, reconnecting...")
		time.Sleep(2 * time.Second)
	}
}

func decodeProgramData(logs []string) {
	// var tokenCreation bool
	for _, logEntry := range logs {

			if !strings.Contains(logEntry, "Program data:") {
				continue
			}

			if !strings.Contains(logEntry, "") {
				continue
			}

			parts := strings.Split(logEntry, "Program data:")
			if len(parts) < 2 {
				continue
			}

			base64Data := strings.TrimSpace(parts[1])
			log.Println("Found base64 program data:", base64Data)

			decodedData, err := base64.StdEncoding.DecodeString(base64Data)
			if err != nil {
				log.Println("Error decoding base64:", err)
				continue
			}

			if len(decodedData) < 72 {
				log.Printf("Decoded data too short: got %d bytes\n", len(decodedData))
				continue
			}

			// Extract the fields from the decoded data
			amount := binary.LittleEndian.Uint64(decodedData[:8])
			mintAddressBytes := decodedData[8:40]
			destinationAddressBytes := decodedData[40:72]

			// Convert to Base58 strings
			mintAddress := base58.Encode(mintAddressBytes)
			destinationAddress := base58.Encode(destinationAddressBytes)

			fmt.Printf("Decoded Token Minting Data:\n")
			fmt.Printf("  Amount: %d\n", amount)
			fmt.Printf("  Mint Address (Base58): %s\n", mintAddress)
			fmt.Printf("  Destination Address (Base58): %s\n", destinationAddress)
		}
}

func main() {
    if err := godotenv.Load(); err != nil {
        log.Fatal("Error loading .env file")
    }

	PORT := os.Getenv("PORT")
    HELIUS_KEY = os.Getenv("HELIUS_KEY")
	if len(HELIUS_KEY) < 8 {
		log.Println("Missing key")
        return
    }
    if PORT == "" {
        PORT = "8080"
    }

	http.HandleFunc("/ws", handleClientWebSocket)

	go listenForSolanaEvents()
	go sendToClients()

	log.Println("WebSocket server listening on :8080...")
	if err := http.ListenAndServe(PORT, nil); err != nil {
		log.Fatal("Error starting server:", err)
	}
}