package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// JSON структуры для сообщений
type Message struct {
	Type      string            `json:"type"`
	Content   string            `json:"content,omitempty"`
	From      string            `json:"from,omitempty"`
	To        string            `json:"to,omitempty"`
	Timestamp string            `json:"timestamp,omitempty"`
	Users     []string          `json:"users,omitempty"`
	Flags     map[string]bool   `json:"flags,omitempty"`
	Error     string            `json:"error,omitempty"`
	Data      map[string]string `json:"data,omitempty"`
}

type Client struct {
	conn            *websocket.Conn
	nickname        string
	address         string
	send            chan Message
	blocked         map[string]bool
	favoriteUsers   map[string]bool
	showWordLengths bool
	showUppercase   bool
	color           string // Hex color for user messages
}

type MailboxMessage struct {
	From    string
	Message string
	Time    time.Time
}

type Mailbox struct {
	Messages []MailboxMessage
	Mutex    sync.RWMutex
}

type ChatServer struct {
	host         string
	port         int
	clients      map[*Client]bool
	mutex        sync.Mutex
	running      bool
	userHistory  map[string]string
	historyMutex sync.RWMutex
	mailboxes    map[string]*Mailbox // никнейм -> почтовый ящик
	mailboxMutex sync.RWMutex
	upgrader     websocket.Upgrader
	logFile      string
	// lastMessages хранит последнее отправленное сообщение для каждого ника
	lastMessages      map[string]Message
	lastMessagesMutex sync.RWMutex
	lastWriter        string
	lastWriteTime     time.Time
	lastWriterMutex   sync.RWMutex
	// Счётчики сообщений
	totalCount   uint64 // все сообщения
	publicCount  uint64 // публичные (общий чат)
	privateCount uint64 // приватные (личные и массовые)
}

func NewChatServer(host string, port int) *ChatServer {
	logFile := "server.log"
	file, err := os.Create(logFile)
	if err != nil {
		log.Fatalf("❌ Не удалось создать лог-файл: %v", err)
	}
	file.Close()

	return &ChatServer{
		host:         host,
		port:         port,
		clients:      make(map[*Client]bool),
		running:      false,
		userHistory:  make(map[string]string),
		mailboxes:    make(map[string]*Mailbox),
		lastMessages: make(map[string]Message),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // Разрешаем подключения с любых источников
			},
		},
		logFile: logFile,
	}
}

// generateRandomColor generates a random hex color
func generateRandomColor() string {
	rand.Seed(time.Now().UnixNano())
	return fmt.Sprintf("#%06X", rand.Intn(0xFFFFFF))
}

// isValidHexColor validates if a string is a valid 6-character hex color
func isValidHexColor(color string) bool {
	matched, _ := regexp.MatchString(`^#[0-9A-Fa-f]{6}$`, color)
	return matched
}

func (s *ChatServer) logToFile(message string) {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	logMessage := fmt.Sprintf("[%s] %s\n", timestamp, message)

	// Log to console and append to log file
	fmt.Print(logMessage)
	file, err := os.OpenFile(s.logFile, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("❌ Ошибка записи в лог-файл: %v\n", err)
		return
	}
	defer file.Close()

	file.WriteString(logMessage)
}

// setLastMessage сохраняет последнее сообщение для данного ника
func (s *ChatServer) setLastMessage(nickname string, msg Message) {
	if nickname == "" {
		return
	}
	s.lastMessagesMutex.Lock()
	defer s.lastMessagesMutex.Unlock()
	s.lastMessages[nickname] = msg
}

// getLastMessage возвращает последнее сообщение для ника и true, если оно найдено
func (s *ChatServer) getLastMessage(nickname string) (Message, bool) {
	s.lastMessagesMutex.RLock()
	defer s.lastMessagesMutex.RUnlock()
	msg, ok := s.lastMessages[nickname]
	return msg, ok
}

func (s *ChatServer) incPublic() {
	atomic.AddUint64(&s.publicCount, 1)
	atomic.AddUint64(&s.totalCount, 1)
}

func (s *ChatServer) incPrivate() {
	atomic.AddUint64(&s.privateCount, 1)
	atomic.AddUint64(&s.totalCount, 1)
}

func (s *ChatServer) statsString() string {
	t := atomic.LoadUint64(&s.totalCount)
	pb := atomic.LoadUint64(&s.publicCount)
	pr := atomic.LoadUint64(&s.privateCount)
	return fmt.Sprintf("Статистика: всего=%d, публичные=%d, приватные=%d", t, pb, pr)
}

// Функции для работы с WebSocket сообщениями
func (s *ChatServer) sendJSONMessage(client *Client, msg Message) error {
	select {
	case client.send <- msg:
		return nil
	default:
		close(client.send)
		return fmt.Errorf("канал отправки заблокирован")
	}
}

func (s *ChatServer) readJSONMessage(conn *websocket.Conn) (*Message, error) {
	_, message, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}

	var msg Message
	err = json.Unmarshal(message, &msg)
	if err != nil {
		return nil, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	return &msg, nil
}

// replaceWordsWithLengths заменяет слова в тексте на их длины
func replaceWordsWithLengths(text string) string {
	// Регулярное выражение для поиска слов (буквы, цифры, дефисы, апострофы)
	wordRegex := regexp.MustCompile(`\b[\p{L}\p{N}'-]+\b`)

	return wordRegex.ReplaceAllStringFunc(text, func(word string) string {
		return strconv.Itoa(len(word))
	})
}

func (s *ChatServer) Start() error {
	address := fmt.Sprintf("%s:%d", s.host, s.port)
	s.running = true

	// Настраиваем HTTP маршруты
	http.HandleFunc("/ws", s.handleWebSocket)
	http.HandleFunc("/", s.handleHome)
	http.HandleFunc("/send-multi", s.handleSendMulti)

	startMessage := fmt.Sprintf("🚀 WebSocket чат-сервер запущен на %s\nWebSocket endpoint: ws://%s/ws\nОжидание подключений...", address, address)
	fmt.Println(startMessage)
	s.logToFile(fmt.Sprintf("Сервер запущен на %s", address))

	// Обработка сигналов для graceful shutdown
	go s.handleSignals()

	// Запускаем HTTP сервер
	return http.ListenAndServe(address, nil)
}

func (s *ChatServer) handleHome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `
<!DOCTYPE html>
<html>
<head>
    <title>WebSocket Chat Server</title>
</head>
<body>
    <h1>WebSocket Chat Server</h1>
    <p>Сервер запущен и готов к подключениям</p>
    <p>WebSocket endpoint: <code>ws://%s:%d/ws</code></p>
</body>
</html>
`, s.host, s.port)
}

func (s *ChatServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("❌ Ошибка обновления до WebSocket: %v", err)
		return
	}

	clientAddr := r.RemoteAddr
	connectionMessage := fmt.Sprintf("📱 Новое WebSocket подключение: %s", clientAddr)
	fmt.Println(connectionMessage)
	s.logToFile(connectionMessage)

	// Создаем клиента
	client := &Client{
		conn:            conn,
		address:         clientAddr,
		send:            make(chan Message, 256),
		blocked:         make(map[string]bool),
		favoriteUsers:   make(map[string]bool),
		showWordLengths: false,
	}

	// Добавляем клиента в список
	s.addClient(client)

	// Запускаем горутины для чтения и записи
	go s.writePump(client)
	go s.readPump(client)
}

// handleSendMulti — HTTP endpoint для отправки сообщения нескольким получателям через запятую
func (s *ChatServer) handleSendMulti(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {

		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "метод не поддерживается, используйте POST"})

		return
	}

	type requestPayload struct {
		From    string            `json:"from"`
		To      string            `json:"to"`
		Content string            `json:"content"`
		Flags   map[string]bool   `json:"flags"`
		Data    map[string]string `json:"data"`
	}

	var payload requestPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("ошибка парсинга JSON: %v", err)})
		return
	}

	payload.From = strings.TrimSpace(payload.From)
	payload.To = strings.TrimSpace(payload.To)
	if payload.From == "" || payload.To == "" || payload.Content == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "обязательные поля: from, to, content"})
		return
	}

	sender := s.findClientByNickname(payload.From)
	if sender == nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("отправитель %s не в сети", payload.From)})
		return
	}

	recipientsRaw := strings.Split(payload.To, ",")
	sent := make([]string, 0)
	saved := make([]string, 0)
	errors := make(map[string]string)

	for _, rcp := range recipientsRaw {
		target := strings.TrimSpace(rcp)
		if target == "" {
			continue
		}
		if target == sender.nickname {
			errors[target] = "нельзя отправить сообщение самому себе"
			continue
		}

		targetClient := s.findClientByNickname(target)
		if targetClient != nil {
			if targetClient.blocked[sender.nickname] {
				errors[target] = "получатель заблокировал вас"
				continue
			}

			timestamp := time.Now().Format("15:04:05")
			msg := Message{
				Type:      "private",
				Content:   payload.Content,
				From:      sender.nickname,
				To:        target,
				Timestamp: timestamp,
				Flags:     map[string]bool{"private": true},
			}
			if payload.Flags != nil {
				// переносим произвольные флаги, не перезаписывая обязательный private=true
				for k, v := range payload.Flags {
					msg.Flags[k] = v
				}
			}
			if targetClient.favoriteUsers[sender.nickname] {
				msg.Flags["favorite"] = true
			}
			_ = s.sendJSONMessage(targetClient, msg)
			sent = append(sent, target)
			fmt.Printf("💌 ЛС (HTTP) от %s к %s: %s\n", sender.nickname, target, payload.Content)
			continue
		}

		if s.addOfflineMessage(target, sender.nickname, payload.Content) {
			saved = append(saved, target)
			fmt.Printf("📮 (HTTP) %s оставил сообщение для %s (оффлайн): %s\n", sender.nickname, target, payload.Content)
		} else {
			errors[target] = "почтовый ящик переполнен (максимум 10 сообщений)"
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"from":          payload.From,
		"sent":          sent,
		"offline_saved": saved,
		"errors":        errors,
	})
}

// writePump отправляет сообщения клиенту
func (s *ChatServer) writePump(client *Client) {
	defer client.conn.Close()

	for {
		select {
		case message, ok := <-client.send:
			client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				client.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := client.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}

			jsonData, err := json.Marshal(message)
			if err != nil {
				log.Printf("❌ Ошибка сериализации JSON: %v", err)
				return
			}

			w.Write(jsonData)

			// Закрываем writer
			if err := w.Close(); err != nil {
				return
			}
		}
	}
}

// readPump читает сообщения от клиента
func (s *ChatServer) readPump(client *Client) {
	defer func() {
		s.disconnectClient(client)
		client.conn.Close()
	}()

	// Сначала обрабатываем аутентификацию
	if !s.handleAuthentication(client) {
		return
	}

	// Основной цикл чтения сообщений
	for {
		msg, err := s.readJSONMessage(client.conn)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("❌ WebSocket ошибка: %v", err)
			}
			break
		}

		s.handleClientMessage(client, msg)
	}
}

func (s *ChatServer) handleAuthentication(client *Client) bool {
	// Извлекаем IP из адреса
	ip := strings.Split(client.address, ":")[0]

	// Проверяем историю для этого IP
	previousNickname := s.getPreviousNickname(ip)

	// Если есть предыдущий никнейм, предлагаем его
	if previousNickname != "" {
		msg := Message{
			Type:    "nick_prompt",
			Content: previousNickname,
		}
		s.sendJSONMessage(client, msg)
		fmt.Printf("📝 Предлагаем никнейм '%s' для IP %s\n", previousNickname, ip)
	} else {
		msg := Message{Type: "nick_request"}
		s.sendJSONMessage(client, msg)
	}

	// Читаем JSON сообщение с никнеймом
	nickMsg, err := s.readJSONMessage(client.conn)
	if err != nil {
		fmt.Printf("❌ Ошибка чтения никнейма от %s: %v\n", client.address, err)
		return false
	}

	if nickMsg.Type != "nick" {
		errorMsg := Message{
			Type:  "error",
			Error: "Ожидается сообщение с никнеймом",
		}
		s.sendJSONMessage(client, errorMsg)
		return false
	}

	nickname := strings.TrimSpace(nickMsg.Content)
	if nickname == "" {
		errorMsg := Message{
			Type:  "error",
			Error: "Никнейм не может быть пустым",
		}
		s.sendJSONMessage(client, errorMsg)
		return false
	}

	// Проверяем, не занят ли никнейм
	if s.isNicknameTaken(nickname) {
		errorMsg := Message{
			Type:  "error",
			Error: "Никнейм уже занят",
		}
		s.sendJSONMessage(client, errorMsg)
		return false
	}

	// Сохраняем в историю
	s.saveNicknameHistory(ip, nickname)

	// Устанавливаем никнейм клиента
	client.nickname = nickname

	// Отправляем подтверждение
	successMsg := Message{Type: "nick_ok"}
	s.sendJSONMessage(client, successMsg)

	// ТОЛЬКО ПОСЛЕ успешной аутентификации доставляем отложенные сообщения
	s.deliverOfflineMessages(client)

	// Уведомляем всех о новом пользователе
	joinMessage := fmt.Sprintf("🟢 %s присоединился к чату", nickname)

	// Добавляем информацию о повторном входе, если применимо
	if previousNickname != "" && previousNickname == nickname {
		joinMessage = fmt.Sprintf("🟢 %s вернулся в чат", nickname)
	}

	s.broadcastJSONMessage(Message{
		Type:      "system",
		Content:   joinMessage,
		Timestamp: time.Now().Format("15:04:05"),
	}, client)
	fmt.Printf("✅ %s (%s) присоединился к чату\n", nickname, client.address)

	// Отправляем список пользователей новому клиенту
	s.sendUserListJSON(client)

	return true
}

func (s *ChatServer) getOrCreateMailbox(nickname string) *Mailbox {
	s.mailboxMutex.Lock()
	defer s.mailboxMutex.Unlock()

	if mailbox, exists := s.mailboxes[nickname]; exists {
		return mailbox
	}

	mailbox := &Mailbox{
		Messages: make([]MailboxMessage, 0),
	}
	s.mailboxes[nickname] = mailbox
	return mailbox
}

func (s *ChatServer) addOfflineMessage(to, from, message string) bool {
	mailbox := s.getOrCreateMailbox(to)
	mailbox.Mutex.Lock()
	defer mailbox.Mutex.Unlock()

	// Проверяем лимит сообщений (максимум 10)
	if len(mailbox.Messages) >= 10 {
		return false // Ящик переполнен
	}

	mailbox.Messages = append(mailbox.Messages, MailboxMessage{
		From:    from,
		Message: message,
		Time:    time.Now(),
	})
	return true
}

func (s *ChatServer) deliverOfflineMessages(client *Client) {
	mailbox := s.getOrCreateMailbox(client.nickname)
	mailbox.Mutex.Lock()
	defer mailbox.Mutex.Unlock()

	if len(mailbox.Messages) == 0 {
		return
	}

	// Доставляем все сообщения
	for _, msg := range mailbox.Messages {
		timestamp := msg.Time.Format("15:04:05")
		content := msg.Message

		// Применяем фильтр длин слов если включен
		if client.showWordLengths {
			content = replaceWordsWithLengths(msg.Message)
		}

		s.sendJSONMessage(client, Message{
			Type:      "offline_message",
			Content:   content,
			From:      msg.From,
			Timestamp: timestamp,
			Flags:     map[string]bool{"offline": true},
		})
	}

	// Уведомляем пользователя
	s.sendJSONMessage(client, Message{
		Type:    "offline_delivered",
		Content: fmt.Sprintf("Вам доставлено %d отложенных сообщений", len(mailbox.Messages)),
	})

	// Очищаем ящик после доставки
	mailbox.Messages = make([]MailboxMessage, 0)
}

func (s *ChatServer) getMailboxStatusJSON(client *Client) {
	mailbox := s.getOrCreateMailbox(client.nickname)
	mailbox.Mutex.RLock()
	defer mailbox.Mutex.RUnlock()

	count := len(mailbox.Messages)
	if count == 0 {
		s.sendJSONMessage(client, Message{
			Type:    "mailbox_status",
			Content: "Ваш почтовый ящик пуст",
		})
	} else {
		s.sendJSONMessage(client, Message{
			Type:    "mailbox_status",
			Content: fmt.Sprintf("У вас %d отложенных сообщений", count),
		})
	}
}

func (s *ChatServer) getPreviousNickname(ip string) string {
	s.historyMutex.RLock()
	defer s.historyMutex.RUnlock()

	return s.userHistory[ip]
}

func (s *ChatServer) saveNicknameHistory(ip, nickname string) {
	s.historyMutex.Lock()
	defer s.historyMutex.Unlock()

	s.userHistory[ip] = nickname
}

func (s *ChatServer) findClientByNickname(nickname string) *Client {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for client := range s.clients {
		if client.nickname == nickname {
			return client
		}
	}
	return nil
}

// updateLastWriter обновляет информацию о последнем писавшем пользователе
func (s *ChatServer) updateLastWriter(nickname string) {
	s.lastWriterMutex.Lock()
	defer s.lastWriterMutex.Unlock()

	s.lastWriter = nickname
	s.lastWriteTime = time.Now()
}

// getLastWriter получает информацию о последнем писавшем пользователе
func (s *ChatServer) getLastWriter() (string, time.Time) {
	s.lastWriterMutex.RLock()
	defer s.lastWriterMutex.RUnlock()

	return s.lastWriter, s.lastWriteTime
}

// handleClientMessage обрабатывает сообщения от клиента
func (s *ChatServer) handleClientMessage(client *Client, msg *Message) {
	switch msg.Type {
	case "message":
		chatMessage := fmt.Sprintf("💬 %s: %s", client.nickname, msg.Content)
		fmt.Println(chatMessage)
		s.logToFile(chatMessage)
		// Обычное сообщение в чат
		// Учитываем режим капса у отправителя
		content := msg.Content
		if client.showUppercase {
			content = strings.ToUpper(content)
		}
		// Сохраняем как последнее сообщение отправителя
		s.setLastMessage(client.nickname, Message{
			Type:      "chat",
			Content:   content,
			From:      client.nickname,
			Timestamp: time.Now().Format("15:04:05"),
			Flags:     msg.Flags,
		})
		// Обновляем информацию о последнем писавшем
		s.updateLastWriter(client.nickname)

		s.broadcastJSONMessage(Message{
			Type:      "chat",
			Content:   content,
			From:      client.nickname,
			Timestamp: time.Now().Format("15:04:05"),
			Flags:     msg.Flags,
		}, client)
		s.incPublic()

	case "private":
		// Личное сообщение (поддержка нескольких получателей через запятую)
		recipientsRaw := strings.Split(msg.To, ",")
		for _, r := range recipientsRaw {
			target := strings.TrimSpace(r)
			if target == "" {
				continue
			}

			if target == client.nickname {
				s.sendJSONMessage(client, Message{
					Type:  "error",
					Error: "Нельзя отправить сообщение самому себе",
				})
				continue
			}
			// Засчитываем приватное сообщение один раз при любой успешной доставке или офлайн-сохранении
			__pvCounted := false
			defer func() {
				if __pvCounted {
					s.incPrivate()
				}
			}()
			// Специальная обработка команд, адресованных встроенному нику 'server'
			lowerTo := strings.ToLower(target)
			if lowerTo == "server" || lowerTo == "agent" {
				// Ожидаем команды вида: last <ник> или #last <ник>
				parts := strings.Fields(msg.Content)
				if len(parts) >= 2 && (strings.ToLower(parts[0]) == "last" || strings.ToLower(parts[0]) == "#last") {
					targetUser := parts[1]
					if lm, ok := s.getLastMessage(targetUser); ok {
						s.sendJSONMessage(client, Message{
							Type:      "last_result",
							Content:   fmt.Sprintf("Последнее сообщение %s: %s", targetUser, lm.Content),
							From:      targetUser,
							Timestamp: lm.Timestamp,
						})
					} else {
						s.sendJSONMessage(client, Message{
							Type:    "last_result",
							Content: fmt.Sprintf("Нет сообщений от %s", targetUser),
						})
					}
				} else {
					s.sendJSONMessage(client, Message{
						Type:  "error",
						Error: "Использование: @server last <ник>",
					})
				}
				continue
			}

			targetClient := s.findClientByNickname(target)
			if targetClient != nil {
				// Проверяем блокировку
				if targetClient.blocked[client.nickname] {
					s.sendJSONMessage(client, Message{
						Type:  "error",
						Error: fmt.Sprintf("Пользователь %s заблокировал вас", target),
					})
					continue
				}

				timestamp := time.Now().Format("15:04:05")
				// Отправляем получателю (учитываем капс у отправителя)
				pcontent := msg.Content
				if client.showUppercase {
					pcontent = strings.ToUpper(pcontent)
				}
				privateMsg := Message{
					Type:      "private",
					Content:   pcontent,
					From:      client.nickname,
					To:        target,
					Timestamp: timestamp,
					Flags:     map[string]bool{"private": true},
				}

				// Добавляем флаг "favorite" если отправитель в списке любимых получателя
				if targetClient.favoriteUsers[client.nickname] {
					privateMsg.Flags["favorite"] = true
				}

				// Добавляем цвет отправителя
				if client.color != "" {
					privateMsg.Data = map[string]string{"color": client.color}
				}

				s.sendJSONMessage(targetClient, privateMsg)
				__pvCounted = true
				// Отправляем подтверждение отправителю
				s.sendJSONMessage(client, Message{
					Type:      "private_sent",
					Content:   msg.Content,
					From:      client.nickname,
					To:        target,
					Timestamp: timestamp,
					Flags:     map[string]bool{"private": true},
				})
				privateMessage := fmt.Sprintf("💌 ЛС от %s к %s: %s", client.nickname, target, msg.Content)
				fmt.Println(privateMessage)
				s.logToFile(privateMessage)
			} else {
				// Пользователь оффлайн - сохраняем как отложенное сообщение (учитывая капс)
				offContent := msg.Content
				if client.showUppercase {
					offContent = strings.ToUpper(offContent)
				}
				// Пользователь оффлайн - сохраняем как отложенное сообщение
				success := s.addOfflineMessage(target, client.nickname, offContent)
				if success {
					__pvCounted = true
					timestamp := time.Now().Format("15:04:05")
					s.sendJSONMessage(client, Message{
						Type:      "offline_saved",
						Content:   fmt.Sprintf("Сообщение для %s сохранено (пользователь оффлайн)", target),
						Timestamp: timestamp,
					})
					fmt.Printf("📮 %s оставил сообщение для %s (оффлайн): %s\n", client.nickname, target, msg.Content)
				} else {
					s.sendJSONMessage(client, Message{
						Type:  "error",
						Error: fmt.Sprintf("Почтовый ящик %s переполнен (максимум 10 сообщений)", target),
					})
				}
			}
		}

	case "command":
		s.handleCommand(client, msg)

	default:
		s.sendJSONMessage(client, Message{
			Type:  "error",
			Error: "Неизвестный тип сообщения",
		})
	}
}

func (s *ChatServer) handleCommand(client *Client, msg *Message) {
	cmd := msg.Data["command"]

	switch cmd {
	case "help":
		s.sendHelpJSON(client)
	case "users":
		s.sendUserListJSON(client)
	case "mailbox":
		s.getMailboxStatusJSON(client)
	case "lastwriter":
		// Команда для вывода последнего писавшего пользователя
		s.sendLastWriterJSON(client)
	case "all":
		content := msg.Data["content"]
		if content == "" {
			s.sendJSONMessage(client, Message{
				Type:  "error",
				Error: "Использование: #all сообщение",
			})
			return
		}
		// Обновляем информацию о последнем писавшем
		s.updateLastWriter(client.nickname)

		timestamp := time.Now().Format("15:04:05")
		// Учитываем режим капса у отправителя
		bcontent := content
		if client.showUppercase {
			bcontent = strings.ToUpper(bcontent)
		}
		// Сохраняем последнее массовое сообщение отправителя
		s.setLastMessage(client.nickname, Message{
			Type:      "mass_private",
			Content:   bcontent,
			From:      client.nickname,
			Timestamp: timestamp,
			Flags:     map[string]bool{"mass_private": true},
		})
		s.broadcastJSONMessage(Message{
			Type:      "mass_private",
			Content:   bcontent,
			From:      client.nickname,
			Timestamp: timestamp,
			Flags:     map[string]bool{"mass_private": true},
		}, client)
		s.sendJSONMessage(client, Message{
			Type:      "mass_private_sent",
			Content:   bcontent,
			From:      client.nickname,
			Timestamp: timestamp,
			Flags:     map[string]bool{"mass_private": true},
		})
		s.incPrivate()

	case "block":
		target := msg.Data["target"]
		if target == "" {
			s.sendJSONMessage(client, Message{
				Type:  "error",
				Error: "Использование: #block ник",
			})
			return
		}
		client.blocked[target] = true
		s.sendJSONMessage(client, Message{
			Type:    "blocked",
			Content: fmt.Sprintf("%s добавлен в чёрный список", target),
		})

	case "unblock":
		target := msg.Data["target"]
		if target == "" {
			s.sendJSONMessage(client, Message{
				Type:  "error",
				Error: "Использование: #unblock ник",
			})
			return
		}
		delete(client.blocked, target)
		s.sendJSONMessage(client, Message{
			Type:    "unblocked",
			Content: fmt.Sprintf("%s убран из чёрного списка", target),
		})

	case "fav":
		action := msg.Data["action"]
		target := msg.Data["target"]

		switch action {
		case "list":
			if len(client.favoriteUsers) == 0 {
				s.sendJSONMessage(client, Message{
					Type:    "fav_list",
					Users:   []string{},
					Content: "Ваш список любимых писателей пуст",
				})
			} else {
				var favList []string
				for user := range client.favoriteUsers {
					favList = append(favList, user)
				}
				s.sendJSONMessage(client, Message{
					Type:    "fav_list",
					Users:   favList,
					Content: fmt.Sprintf("Ваши любимые писатели (%d): %s", len(favList), strings.Join(favList, ", ")),
				})
			}
		case "clear":
			client.favoriteUsers = make(map[string]bool)
			s.sendJSONMessage(client, Message{
				Type:    "fav_cleared",
				Content: "Список любимых писателей очищен",
			})
		case "add", "remove":
			if target == "" {
				s.sendJSONMessage(client, Message{
					Type:  "error",
					Error: "Укажите никнейм пользователя",
				})
				return
			}

			if target == client.nickname {
				s.sendJSONMessage(client, Message{
					Type:  "error",
					Error: "Нельзя добавить себя в любимые писатели",
				})
				return
			}

			if !s.isNicknameTaken(target) {
				s.sendJSONMessage(client, Message{
					Type:  "error",
					Error: fmt.Sprintf("Пользователь %s не найден", target),
				})
				return
			}

			if action == "add" {
				if client.favoriteUsers[target] {
					s.sendJSONMessage(client, Message{
						Type:  "error",
						Error: fmt.Sprintf("%s уже в списке любимых писателей", target),
					})
				} else {
					client.favoriteUsers[target] = true
					s.sendJSONMessage(client, Message{
						Type:    "fav_added",
						Content: fmt.Sprintf("%s добавлен в список любимых писателей", target),
						Data:    map[string]string{"user": target},
					})
				}
			} else { // remove
				if !client.favoriteUsers[target] {
					s.sendJSONMessage(client, Message{
						Type:  "error",
						Error: fmt.Sprintf("%s не в списке любимых писателей", target),
					})
				} else {
					delete(client.favoriteUsers, target)
					s.sendJSONMessage(client, Message{
						Type:    "fav_removed",
						Content: fmt.Sprintf("%s удален из списка любимых писателей", target),
						Data:    map[string]string{"user": target},
					})
				}
			}
		default:
			s.sendJSONMessage(client, Message{
				Type:  "error",
				Error: "Неизвестная команда fav",
			})
		}
	case "last":
		// Ожидается msg.Data["target"] = ник
		target := msg.Data["target"]
		if target == "" {
			s.sendJSONMessage(client, Message{
				Type:  "error",
				Error: "Использование: #last <ник>",
			})
			return
		}
		if lm, ok := s.getLastMessage(target); ok {
			s.sendJSONMessage(client, Message{
				Type:      "last_result",
				Content:   lm.Content,
				From:      lm.From,
				Timestamp: lm.Timestamp,
				Data:      map[string]string{"type": lm.Type},
			})
		} else {
			s.sendJSONMessage(client, Message{
				Type:    "last_result",
				Content: fmt.Sprintf("Нет сообщений от %s", target),
			})
		}
	case "wordlengths":
		client.showWordLengths = !client.showWordLengths
		status := "выключен"
		if client.showWordLengths {
			status = "включен"
		}
		s.sendJSONMessage(client, Message{
			Type:    "wordlengths_toggle",
			Content: fmt.Sprintf("Режим показа длин слов %s", status),
		})

	case "upper":
		client.showUppercase = !client.showUppercase
		status := "выключен"
		if client.showUppercase {
			status = "включен"
		}
		s.sendJSONMessage(client, Message{
			Type:    "upper_toggle",
			Content: fmt.Sprintf("Режим капса %s", status),
		})
	case "log":
		s.sendLogFile(client)
	case "stats":
		s.sendJSONMessage(client, Message{
			Type:    "stats",
			Content: s.statsString(),
		})
		return
	case "kick":
		targetNick := strings.TrimSpace(msg.Data["target"])
		reason := strings.TrimSpace(msg.Data["reason"]) // optional
		if targetNick == "" {
			s.sendJSONMessage(client, Message{
				Type:  "error",
				Error: "Использование: #kick <ник> [причина]",
			})
			return
		}
		if targetNick == client.nickname {
			s.sendJSONMessage(client, Message{
				Type:  "error",
				Error: "Нельзя кикнуть себя",
			})
			return
		}
		target := s.findClientByNickname(targetNick)
		if target == nil {
			s.sendJSONMessage(client, Message{
				Type:  "error",
				Error: fmt.Sprintf("Пользователь %s не найден", targetNick),
			})
			return
		}
		s.kickClient(target, client.nickname, reason)
		s.sendJSONMessage(client, Message{
			Type:    "info",
			Content: fmt.Sprintf("Пользователь %s кикнут", targetNick),
		})
	case "color":
		target := msg.Data["target"]
		if target == "" {
			// Random color
			client.color = generateRandomColor()
			s.sendJSONMessage(client, Message{
				Type:    "color_set",
				Content: fmt.Sprintf("Цвет текста сообщений установлен: %s", client.color),
				Data:    map[string]string{"color": client.color},
			})
		} else {
			// Validate hex color
			if !isValidHexColor(target) {
				s.sendJSONMessage(client, Message{
					Type:  "error",
					Error: "Неверный формат цвета. Используйте #RRGGBB (например, #FF0000)",
				})
				return
			}
			client.color = strings.ToUpper(target)
			s.sendJSONMessage(client, Message{
				Type:    "color_set",
				Content: fmt.Sprintf("Цвет текста сообщений установлен: %s", client.color),
				Data:    map[string]string{"color": client.color},
			})
		}

	default:
		s.sendJSONMessage(client, Message{
			Type:  "error",
			Error: "Неизвестная команда",
		})
	}
}

// kickClient принудительно отключает пользователя с уведомлением и логированием
func (s *ChatServer) kickClient(target *Client, by string, reason string) {
	if target == nil {
		return
	}
	if reason == "" {
		reason = "без причины"
	}

	timestamp := time.Now().Format("15:04:05")

	// Уведомляем целевого пользователя
	s.sendJSONMessage(target, Message{
		Type:      "system",
		Content:   fmt.Sprintf("Вас кикнул %s: %s", by, reason),
		Timestamp: timestamp,
		Flags:     map[string]bool{"kicked": true},
	})

	// Логируем и уведомляем остальных
	info := fmt.Sprintf("⛔ %s кикнул %s: %s", by, target.nickname, reason)
	fmt.Println(info)
	s.logToFile(info)
	s.broadcastJSONMessage(Message{
		Type:      "system",
		Content:   fmt.Sprintf("⛔ %s был кикнут (%s)", target.nickname, reason),
		Timestamp: timestamp,
	}, target)

	// Отключаем пользователя
	s.disconnectClient(target)
}

// sendLastWriterJSON отправляет информацию о последнем писавшем пользователе
func (s *ChatServer) sendLastWriterJSON(client *Client) {
	lastWriter, lastWriteTime := s.getLastWriter()

	if lastWriter == "" {
		s.sendJSONMessage(client, Message{
			Type:    "last_writer",
			Content: "Пока никто не писал в чат",
		})
	} else {
		timeStr := lastWriteTime.Format("15:04:05")
		s.sendJSONMessage(client, Message{
			Type:      "last_writer",
			Content:   fmt.Sprintf("Последний писавший: %s в %s", lastWriter, timeStr),
			From:      lastWriter,
			Timestamp: timeStr,
		})
	}
}

// broadcastJSONMessage рассылает сообщение всем клиентам
func (s *ChatServer) sendLogFile(client *Client) {
	content, err := ioutil.ReadFile(s.logFile)
	if err != nil {
		s.sendJSONMessage(client, Message{
			Type:  "error",
			Error: "Не удалось прочитать лог-файл",
		})
		return
	}

	s.sendJSONMessage(client, Message{
		Type:    "log",
		Content: string(content),
	})
}

func (s *ChatServer) broadcastJSONMessage(msg Message, exclude *Client) {
	s.mutex.Lock()
	clients := make(map[*Client]bool)
	for client := range s.clients {
		clients[client] = true
	}
	s.mutex.Unlock()

	var disconnected []*Client

	for client := range clients {
		if exclude != nil && client == exclude {
			continue
		}

		// Проверяем блокировку для личных и массовых сообщений
		if (msg.Type == "private" || msg.Type == "mass_private") && client.blocked[msg.From] {
			continue
		}

		// Создаем копию сообщения для каждого клиента
		clientMsg := msg

		// Применяем фильтр длин слов если включен
		if client.showWordLengths && (msg.Type == "chat" || msg.Type == "private" || msg.Type == "mass_private") {
			clientMsg.Content = replaceWordsWithLengths(msg.Content)
		}

		// Добавляем флаг "favorite" если отправитель в списке любимых получателя
		if (msg.Type == "chat" || msg.Type == "mass_private") && client.favoriteUsers[msg.From] {
			if clientMsg.Flags == nil {
				clientMsg.Flags = make(map[string]bool)
			}
			clientMsg.Flags["favorite"] = true
		}

		// Добавляем цвет отправителя в Data
		if sender := s.findClientByNickname(msg.From); sender != nil && sender.color != "" {
			if clientMsg.Data == nil {
				clientMsg.Data = make(map[string]string)
			}
			clientMsg.Data["color"] = sender.color
		}

		err := s.sendJSONMessage(client, clientMsg)
		if err != nil {
			fmt.Printf("❌ Ошибка отправки сообщения %s: %v\n", client.nickname, err)
			disconnected = append(disconnected, client)
		}
	}

	// Удаляем отключившихся клиентов
	for _, client := range disconnected {
		s.removeClient(client)
		fmt.Printf("🔴 %s отключился (потеряна связь)\n", client.nickname)
		client.conn.Close()
	}
}

func (s *ChatServer) sendHelpJSON(client *Client) {
	helpData := map[string]string{
		"@ник сообщение":      "личное сообщение",
		"#all сообщение":      "массовое личное сообщение",
		"#users":              "список пользователей",
		"#help":               "эта справка",
		"#mailbox":            "проверить почтовый ящик",
		"#lastwriter":         "показать последнего писавшего пользователя",
		"#fav [ник]":          "добавить/удалить любимого писателя",
		"#fav list":           "показать список",
		"#fav clear":          "очистить список",
		"#block ник":          "добавить в чёрный список",
		"#unblock ник":        "убрать из чёрного списка",
		"#color":              "установить случайный цвет текста сообщений",
		"#color #hex":         "установить цвет текста сообщений (например, #FF0000)",
		"#log":                "получить содержимое лог-файла",
		"stats":               "#stats — статистика сообщений",
		"#wordlengths":        "переключить режим показа длин слов",
		"#kick ник [причина]": "кикнуть пользователя с указанием причины",
		"#upper":              "отображать ваши сообщения в верхнем регистре",
		"/quit":               "выход из чата",
	}

	s.sendJSONMessage(client, Message{
		Type: "help",
		Data: helpData,
	})
}
func (s *ChatServer) isNicknameTaken(nickname string) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for client := range s.clients {
		if client.nickname == nickname {
			return true
		}
	}
	return false
}

func (s *ChatServer) addClient(client *Client) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.clients[client] = true
}

func (s *ChatServer) removeClient(client *Client) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	delete(s.clients, client)
}

func (s *ChatServer) sendUserListJSON(client *Client) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	var users []string
	for c := range s.clients {
		users = append(users, c.nickname)
	}

	s.sendJSONMessage(client, Message{
		Type:  "users",
		Users: users,
	})
}

func (s *ChatServer) disconnectClient(client *Client) {
	s.removeClient(client)
	client.conn.Close()

	if client.nickname != "" {
		leaveMessage := fmt.Sprintf("🔴 %s покинул чат", client.nickname)
		fmt.Println(leaveMessage)
		s.logToFile(leaveMessage)
		s.broadcastJSONMessage(Message{
			Type:      "system",
			Content:   leaveMessage,
			Timestamp: time.Now().Format("15:04:05"),
		}, nil)
	}
}

func (s *ChatServer) userExistsInHistory(nickname string) bool {
	s.historyMutex.RLock()
	defer s.historyMutex.RUnlock()

	for _, storedNickname := range s.userHistory {
		if storedNickname == nickname {
			return true
		}
	}
	return false
}

func (s *ChatServer) Shutdown() {
	if !s.running {
		return
	}

	s.running = false
	fmt.Println("\n🛑 Остановка сервера...")

	// Закрываем все клиентские соединения
	s.mutex.Lock()
	for client := range s.clients {
		close(client.send)
		client.conn.Close()
	}
	s.clients = make(map[*Client]bool)
	s.mutex.Unlock()

	// Удаляем лог-файл
	err := os.Remove(s.logFile)
	if err != nil {
		fmt.Printf("❌ Ошибка удаления лог-файла: %v\n", err)
	} else {
		fmt.Println("🗑️ Лог-файл удалён")
	}

	fmt.Println("✅ Сервер остановлен")
}

func (s *ChatServer) handleSignals() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	<-sigChan
	fmt.Println("\n🛑 Получен сигнал остановки...")
	s.Shutdown()
	os.Exit(0)
}

func getServerConfig() (string, int) {
	// Проверяем переменные окружения
	host := os.Getenv("SERVER_HOST")
	portStr := os.Getenv("SERVER_PORT")

	// Если переменные окружения не заданы, используем интерактивный ввод
	if host == "" && portStr == "" {
		reader := bufio.NewReader(os.Stdin)

		fmt.Println("=== 💬 WebSocket чат-сервер (Go) ===")
		fmt.Println("Введите адрес сервера (по умолчанию: 0.0.0.0)")
		fmt.Print("Адрес сервера [0.0.0.0]: ")

		hostInput, _ := reader.ReadString('\n')
		hostInput = strings.TrimSpace(hostInput)

		if hostInput == "" {
			hostInput = "0.0.0.0"
		}

		fmt.Print("Введите порт сервера (по умолчанию: 12345): ")
		portInput, _ := reader.ReadString('\n')
		portInput = strings.TrimSpace(portInput)

		var port int
		if portInput == "" {
			port = 12345
		} else {
			if _, err := fmt.Sscanf(portInput, "%d", &port); err != nil || port <= 0 || port >= 65536 {
				fmt.Printf("❌ Неверный порт, используем порт по умолчанию: 12345\n")
				port = 12345
			}
		}

		return hostInput, port
	}

	// Используем переменные окружения
	if host == "" {
		host = "0.0.0.0"
	}

	var port int
	if portStr == "" {
		port = 12345
	} else {
		if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil || port <= 0 || port >= 65536 {
			fmt.Printf("❌ Неверный порт в переменной окружения SERVER_PORT, используем порт по умолчанию: 12345\n")
			port = 12345
		}
	}

	fmt.Println("=== 💬 WebSocket чат-сервер (Go) ===")
	fmt.Printf("Используются настройки из переменных окружения:\n")
	fmt.Printf("Адрес сервера: %s\n", host)
	fmt.Printf("Порт сервера: %d\n", port)

	return host, port
}

func main() {
	host, port := getServerConfig()

	fmt.Printf("🚀 Запуск сервера на %s:%d\n", host, port)
	fmt.Println("Для остановки нажмите Ctrl+C")
	fmt.Println()

	server := NewChatServer(host, port)

	err := server.Start()
	if err != nil {
		fmt.Printf("❌ Ошибка: %v\n", err)
		os.Exit(1)
	}
}
