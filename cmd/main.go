package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"github.com/mr-tron/base58"
)

type ClientStatus struct {
	clients map[*websocket.Conn]bool
	mu      sync.Mutex
}

type ClientMessage struct {
	SubscriptionType string `json:"subscriptionType"`
}

const (
	divideByLamports   uint64 = 1_000_000_000
	solanaWebSocketURL string = "wss://mainnet.helius-rpc.com/?api-key="
)

var (
	HELIUS_KEY string

	solConn *websocket.Conn

	subscriptionMap map[string][]*websocket.Conn

	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	clientStatus = ClientStatus{
		clients: make(map[*websocket.Conn]bool),
	}
)

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
		_, msg, err := conn.ReadMessage()
		var message string
		err = json.Unmarshal(msg, &message)
		if err != nil {
			log.Println("Client disconnected:", err)
			break
		}
		if len(message) == 32 && strings.Contains(message, "pump") {
			handleSubscribe(conn, message)
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

func handleSubscribe(clientAddress *websocket.Conn, subscriptionAddress string) {

	subscriptionMap[subscriptionAddress] = append(subscriptionMap[subscriptionAddress], clientAddress)

	err := clientAddress.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("Successfully subscribed to %v", subscriptionAddress)))
	if err != nil {
		log.Println("Error sending message to client:", err)
		clientAddress.Close()

		subscriptionMap[subscriptionAddress] = removeItems(subscriptionMap[subscriptionAddress], clientAddress)

		if len(subscriptionMap[subscriptionAddress]) == 0 {
			delete(subscriptionMap, subscriptionAddress)
		}
	}
}

func removeItems(arr []*websocket.Conn, value *websocket.Conn) []*websocket.Conn {
	var newArr []*websocket.Conn
	for _, v := range arr {
		if v != value {
			newArr = append(newArr, v)
		}
	}
	return newArr
}

func solanaWSConnection() (*websocket.Conn, error) {
	conn, r, err := websocket.DefaultDialer.Dial(solanaWebSocketURL+string(HELIUS_KEY), nil)
	solConn = conn
	if err != nil {
		return nil, err
	}
	log.Println("Connected to Solana WebSocket. Status:", r.StatusCode)

	conn.SetPingHandler(func(appData string) error {
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})

	return solConn, nil
}

func solanaLogsSubscriber(conn *websocket.Conn) {
	subscriptionMessage := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "logsSubscribe",
		"params": []interface{}{
			map[string]interface{}{
				"mentions": []string{"6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P"},
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

func solanaEventListener() {
	for {
		conn, err := solanaWSConnection()
		if err != nil {
			log.Println("Error connecting to Solana WebSocket:", err)
			time.Sleep(5 * time.Second)
			continue
		}
		solanaLogsSubscriber(conn)

		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				log.Println("Error reading from Solana WebSocket:", err)
				break
			}

			var messageLogs Result
			if err = json.Unmarshal(msg, &messageLogs); err != nil {
				continue
			}
			programData := txFilter(messageLogs.Params.Result.Value.Logs)
			if programData == "" {
				continue
			}
			messageChannel <- string(msg)
		}

		conn.Close()
		log.Println("Disconnected from Solana WebSocket, reconnecting...")
		time.Sleep(2 * time.Second)
	}
}

func txFilter(logs []string) string {
	var mint string
	var solAmount uint64
	var destinationAddress string
	var buyOrSell string
	var create bool
	var err error

	for _, logEntry := range logs {
		if strings.Contains(logEntry, "Buy") {
			buyOrSell = "Buy"
		}
		if strings.Contains(logEntry, "Sell") {
			buyOrSell = "Sell"
		}

		if strings.Contains(logEntry, "InitializeMint") {
			create = true
		}

		if mint != "" && buyOrSell != "" && create {
			break
		}

		if !strings.Contains(logEntry, "") || !strings.Contains(logEntry, "Program data:") {
			continue
		}

		parts := strings.Split(logEntry, "Program data:")
		if len(parts) < 2 {
			continue
		}
		if solAmount, mint, destinationAddress, err = decodeProgramData(parts[1]); err != nil {
			return ""
		}
	}

	if create && mint != "" {
		fmt.Printf("\n============================================")
		fmt.Printf("\n  Amount: %d\n", solAmount)
		fmt.Printf("  Mint Address (Base58): %s\n", mint)
		fmt.Printf("  Destination Address (Base58): %s\n", destinationAddress)
		fmt.Printf("============================================\n")
		return "Create"
	} else if buyOrSell == "Buy" && !create && mint != "" && strings.Contains(mint, "pump") {
		fmt.Printf("\n============================================")
		fmt.Println("\n", buyOrSell, "\n", mint)
		fmt.Printf("============================================\n")
		return buyOrSell
	} else if buyOrSell == "Sell" && !create && mint != "" && strings.Contains(mint, "pump") {
		fmt.Printf("\n============================================")
		fmt.Println("\n", buyOrSell, "\n", mint)
		fmt.Printf("============================================\n")
		return buyOrSell
	}

	return ""
}

func decodeProgramData(encodedData string) (amount uint64, mint, Destination string, error error) {

	base64Data := strings.TrimSpace(encodedData)

	decodedData, err := base64.StdEncoding.DecodeString(base64Data)

	if err != nil || len(decodedData) < 72 {
		return 0, "", "", err
	}

	// Extract the fields from the decoded data
	mintAddressBytes := decodedData[8:40]

	// Convert to Base58 strings
	mintAddress := base58.Encode(mintAddressBytes)
	if !strings.Contains(mintAddress, "pump") {
		return 0, "", "", err
	}

	destinationAddressBytes := decodedData[40:72]
	destinationAddress := base58.Encode(destinationAddressBytes)

	amount = binary.LittleEndian.Uint64(decodedData[:8])
	solAmount := amount / divideByLamports

	return solAmount, mintAddress, destinationAddress, nil
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

	go solanaEventListener()
	go sendToClients()

	log.Println("WebSocket server listening on :8080...")
	if err := http.ListenAndServe(PORT, nil); err != nil {
		log.Fatal("Error starting server:", err)
	}
}
