package main

import (
	"encoding/json"
	"fmt"
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

/*
 Протокол сообщений (совместим с клиентом):
 - Все сообщения JSON с полями: type, content, from, to, timestamp, users, flags, error, data
 - Публичный чат:        type="message" от клиента, сервер рассылает type="chat"
 - Личные сообщения:     type="private" от клиента, сервер -> адресату type="private", отправителю type="privatesent"
 - Массовые приватные:   команда #all -> type="command" {data:{command:"all", content:"..."}}; сервер -> всем type="massprivate", отправителю type="massprivatesent"
 - Команды:
     #help     -> type="command" {data:{command:"help"}}          -> type="help"
     #users    -> type="command" {data:{command:"users"}}         -> type="users"
     #mailbox  -> type="command" {data:{command:"mailbox"}}       -> type="mailboxstatus"
     #stats    -> type="command" {data:{command:"stats"}}         -> type="stats"
     block/unblock/fav ... аналогично данным клиента
 - Ник: клиент шлёт type="nick", content="<ник>"; сервер отвечает "nickok" либо "error" + "nickrequest"/"nickprompt"
*/

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
	color           string // Hex color for user messages (опционально)
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
	host string
	port int

	clients map[*Client]bool
	mutex   sync.Mutex
	running bool

	// ник -> офлайн сообщения
	mailboxes    map[string]*Mailbox
	mailboxMutex sync.RWMutex

	// последнее сообщение по нику (опционально)
	lastMessages      map[string]Message
	lastMessagesMutex sync.RWMutex

	// счётчики
	totalCount   uint64 // все сообщения
	publicCount  uint64 // публичные (общий чат)
	privateCount uint64 // приватные (личные и #all)

	upgrader websocket.Upgrader
}

func NewChatServer(host string, port int) *ChatServer {
	return &ChatServer{
		host:         host,
		port:         port,
		clients:      make(map[*Client]bool),
		mailboxes:    make(map[string]*Mailbox),
		lastMessages: make(map[string]Message),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

// --- вспомогательные утилиты ---

func generateRandomColor() string {
	rand.Seed(time.Now().UnixNano())
	return fmt.Sprintf("#%06X", rand.Intn(0xFFFFFF))
}

func isValidHexColor(color string) bool {
	matched, _ := regexp.MatchString(`^#[0-9A-Fa-f]{6}$`, color)
	return matched
}

func replaceWordsWithLengths(text string) string {
	wordRegex := regexp.MustCompile(`\b[\p{L}\p{N}'-]+\b`)
	return wordRegex.ReplaceAllStringFunc(text, func(word string) string {
		return strconv.Itoa(len(word))
	})
}

func (s *ChatServer) setLastMessage(nickname string, msg Message) {
	if nickname == "" {
		return
	}
	s.lastMessagesMutex.Lock()
	s.lastMessages[nickname] = msg
	s.lastMessagesMutex.Unlock()
}

func (s *ChatServer) getLastMessage(nickname string) (Message, bool) {
	s.lastMessagesMutex.RLock()
	defer s.lastMessagesMutex.RUnlock()
	msg, ok := s.lastMessages[nickname]
	return msg, ok
}

// --- счётчики ---

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

// --- старт/остановка ---

func (s *ChatServer) Start() error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)

	http.HandleFunc("/ws", s.handleWebSocket)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(fmt.Sprintf("WebSocket endpoint: ws://%s/ws\n", addr)))
	})

	// graceful shutdown
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		s.shutdown()
		os.Exit(0)
	}()

	log.Printf("🚀 WebSocket чат-сервер запущен на %s (ws://%s/ws)\n", addr, addr)
	return http.ListenAndServe(addr, nil)
}

func (s *ChatServer) shutdown() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	for c := range s.clients {
		close(c.send)
		_ = c.conn.Close()
	}
}

// --- вебсокет обработка ---

func (s *ChatServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, "upgrade failed", http.StatusBadRequest)
		return
	}

	client := &Client{
		conn:          conn,
		address:       r.RemoteAddr,
		send:          make(chan Message, 256),
		blocked:       make(map[string]bool),
		favoriteUsers: make(map[string]bool),
		color:         generateRandomColor(),
	}

	// регистрируем
	s.registerClient(client)

	// писатель
	go s.writer(client)
	// читатель (блокирующий)
	s.reader(client)
}

func (s *ChatServer) writer(client *Client) {
	for msg := range client.send {
		if err := client.conn.WriteJSON(msg); err != nil {
			break
		}
	}
	_ = client.conn.Close()
}

func (s *ChatServer) reader(client *Client) {
	defer func() {
		s.unregisterClient(client)
	}()

	// запрашиваем ник
	s.sendJSON(client, Message{Type: "nickprompt", Content: "Введите ник"})

	for {
		_, raw, err := client.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			s.sendJSON(client, Message{Type: "error", Error: "Некорректный JSON"})
			continue
		}
		s.handleClientMessage(client, msg)
	}
}

func (s *ChatServer) registerClient(client *Client) {
	s.mutex.Lock()
	s.clients[client] = true
	s.mutex.Unlock()
}

func (s *ChatServer) unregisterClient(client *Client) {
	s.mutex.Lock()
	if _, ok := s.clients[client]; ok {
		delete(s.clients, client)
	}
	s.mutex.Unlock()

	close(client.send)

	// обновим список пользователей
	s.broadcastUsers()
	// системное сообщение (если ник известен)
	if client.nickname != "" {
		s.broadcastSystem(fmt.Sprintf("%s вышел из чата", client.nickname))
	}
}

// --- отправки ---

func (s *ChatServer) sendJSON(c *Client, msg Message) {
	select {
	case c.send <- msg:
	default:
		// клиент не принимает — отключаем
		close(c.send)
		_ = c.conn.Close()
	}
}

func copyFlags(m map[string]bool) map[string]bool {
	if m == nil {
		return nil
	}
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (s *ChatServer) broadcastJSONMessage(msg Message, except *Client) {
	// Для флага избранных сообщения кастомизируем под каждого получателя
	s.mutex.Lock()
	clientsCopy := make([]*Client, 0, len(s.clients))
	for c := range s.clients {
		clientsCopy = append(clientsCopy, c)
	}
	s.mutex.Unlock()

	for _, c := range clientsCopy {
		if except != nil && c == except {
			continue
		}
		per := msg
		// проставим флаг favorite для получателя
		if msg.From != "" {
			if per.Flags == nil {
				per.Flags = make(map[string]bool)
			}
			if c.favoriteUsers[msg.From] {
				per.Flags["favorite"] = true
			}
		}
		s.sendJSON(c, per)
	}
}

func (s *ChatServer) broadcastSystem(text string) {
	s.broadcastJSONMessage(Message{Type: "system", Content: text}, nil)
}

func (s *ChatServer) broadcastUsers() {
	users := s.userList()
	s.broadcastJSONMessage(Message{Type: "users", Users: users}, nil)
}

func (s *ChatServer) userList() []string {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	users := make([]string, 0, len(s.clients))
	for c := range s.clients {
		if c.nickname != "" {
			users = append(users, c.nickname)
		}
	}
	return users
}

func (s *ChatServer) findClientByNick(nick string) *Client {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	for c := range s.clients {
		if c.nickname == nick {
			return c
		}
	}
	return nil
}

// --- офлайн сообщения ---

func (s *ChatServer) addOfflineMessage(to, from, content string) bool {
	if strings.TrimSpace(to) == "" {
		return false
	}
	s.mailboxMutex.Lock()
	defer s.mailboxMutex.Unlock()
	mb := s.mailboxes[to]
	if mb == nil {
		mb = &Mailbox{}
		s.mailboxes[to] = mb
	}
	mb.Mutex.Lock()
	mb.Messages = append(mb.Messages, MailboxMessage{
		From:    from,
		Message: content,
		Time:    time.Now(),
	})
	mb.Mutex.Unlock()
	return true
}

func (s *ChatServer) deliverOfflineMessages(client *Client) {
	s.mailboxMutex.RLock()
	mb := s.mailboxes[client.nickname]
	s.mailboxMutex.RUnlock()
	if mb == nil {
		return
	}
	mb.Mutex.Lock()
	defer mb.Mutex.Unlock()

	if len(mb.Messages) == 0 {
		return
	}
	for _, mm := range mb.Messages {
		s.sendJSON(client, Message{
			Type:      "offlinemessage",
			From:      mm.From,
			Content:   mm.Message,
			Timestamp: mm.Time.Format("15:04:05"),
		})
	}
	count := len(mb.Messages)
	mb.Messages = nil
	s.sendJSON(client, Message{
		Type:    "offlinedelivered",
		Content: fmt.Sprintf("Доставлено офлайн сообщений: %d", count),
	})
}

// --- обработка входящих ---

func (s *ChatServer) handleClientMessage(client *Client, msg Message) {
	switch strings.TrimSpace(strings.ToLower(msg.Type)) {
	case "nick":
		s.handleNick(client, msg)
	case "message":
		s.handlePublicMessage(client, msg)
	case "private":
		s.handlePrivateMessage(client, msg)
	case "command":
		s.handleCommand(client, msg)
	default:
		s.sendJSON(client, Message{Type: "error", Error: "Неизвестный тип сообщения"})
	}
}

func (s *ChatServer) handleNick(client *Client, msg Message) {
	nick := strings.TrimSpace(msg.Content)
	if nick == "" {
		s.sendJSON(client, Message{Type: "error", Error: "Ник не может быть пустым"})
		s.sendJSON(client, Message{Type: "nickrequest", Content: "Введите ник"})
		return
	}
	// проверим уникальность
	if s.findClientByNick(nick) != nil {
		s.sendJSON(client, Message{Type: "error", Error: "Ник уже занят"})
		s.sendJSON(client, Message{Type: "nickrequest", Content: "Введите другой ник"})
		return
	}

	client.nickname = nick
	s.sendJSON(client, Message{Type: "nickok", Content: "OK"})

	// приветствие и список пользователей
	s.broadcastSystem(fmt.Sprintf("%s вошёл в чат", client.nickname))
	s.broadcastUsers()

	// офлайн сообщения
	s.deliverOfflineMessages(client)
}

func (s *ChatServer) handlePublicMessage(client *Client, msg Message) {
	if client.nickname == "" {
		s.sendJSON(client, Message{Type: "error", Error: "Сначала установите ник"})
		return
	}
	content := strings.TrimSpace(msg.Content)
	if content == "" {
		return
	}
	if client.showWordLengths {
		content = replaceWordsWithLengths(content)
	}

	ts := time.Now().Format("15:04:05")
	out := Message{
		Type:      "chat",
		From:      client.nickname,
		Content:   content,
		Timestamp: ts,
	}

	// запомним как последнее сообщение автора
	s.setLastMessage(client.nickname, out)

	// инкремент счётчика публичных
	s.incPublic()

	// рассылаем
	s.broadcastJSONMessage(out, nil)
}

func (s *ChatServer) handlePrivateMessage(client *Client, msg Message) {
	if client.nickname == "" {
		s.sendJSON(client, Message{Type: "error", Error: "Сначала установите ник"})
		return
	}
	to := strings.TrimSpace(msg.To)
	content := strings.TrimSpace(msg.Content)
	if to == "" || content == "" {
		s.sendJSON(client, Message{Type: "error", Error: "Укажите получателя и текст"})
		return
	}
	if to == client.nickname {
		s.sendJSON(client, Message{Type: "error", Error: "Нельзя отправить сообщение самому себе"})
		return
	}

	target := s.findClientByNick(to)
	// если адресат онлайн и не заблокировал отправителя
	if target != nil && !target.blocked[client.nickname] {
		ts := time.Now().Format("15:04:05")
		// адресату
		s.sendJSON(target, Message{
			Type:      "private",
			From:      client.nickname,
			Content:   content,
			Timestamp: ts,
		})
		// отправителю подтверждение
		s.sendJSON(client, Message{
			Type:    "privatesent",
			To:      to,
			Content: content,
		})

		// приватный счетчик
		s.incPrivate()
		return
	}

	// офлайн
	if s.addOfflineMessage(to, client.nickname, content) {
		s.sendJSON(client, Message{
			Type:    "offlinesaved",
			Content: fmt.Sprintf("Сообщение сохранено для %s", to),
		})
		// приватный счетчик (считаем фактом отправки)
		s.incPrivate()
		return
	}

	s.sendJSON(client, Message{Type: "error", Error: "Не удалось доставить сообщение"})
}

func (s *ChatServer) handleCommand(client *Client, msg Message) {
	if client.nickname == "" && (msg.Data == nil || msg.Data["command"] != "help") {
		// только help разрешим без ника
		s.sendJSON(client, Message{Type: "error", Error: "Сначала установите ник"})
		return
	}

	cmd := ""
	if msg.Data != nil {
		cmd = strings.ToLower(strings.TrimSpace(msg.Data["command"]))
	}
	switch cmd {
	case "help":
		// вернём help текстом
		help := strings.Join([]string{
			"Доступные команды:",
			"#users — список пользователей",
			"#mailbox — статус офлайн-почты",
			"#stats — статистика сообщений",
			"block <nick> — блокировать личные сообщения от пользователя",
			"unblock <nick> — снять блокировку",
			"fav list|clear|add <nick>|remove <nick> — управление избранными",
			"#all <msg> — приватно всем (кроме себя)",
		}, "\n")
		s.sendJSON(client, Message{Type: "help", Content: help})
	case "users":
		s.sendJSON(client, Message{Type: "users", Users: s.userList()})
	case "mailbox":
		count := 0
		s.mailboxMutex.RLock()
		mb := s.mailboxes[client.nickname]
		s.mailboxMutex.RUnlock()
		if mb != nil {
			mb.Mutex.RLock()
			count = len(mb.Messages)
			mb.Mutex.RUnlock()
		}
		s.sendJSON(client, Message{
			Type:    "mailboxstatus",
			Content: fmt.Sprintf("Ожидают офлайн сообщений: %d", count),
		})
	case "stats":
		s.sendJSON(client, Message{
			Type:    "stats",
			Content: s.statsString(),
		})
	case "all":
		content := ""
		if msg.Data != nil {
			content = strings.TrimSpace(msg.Data["content"])
		}
		if content == "" {
			s.sendJSON(client, Message{Type: "error", Error: "Пустое сообщение"})
			return
		}
		delivered := 0
		for _, rcpt := range s.snapshotClients() {
			if rcpt == client {
				continue
			}
			if rcpt.blocked[client.nickname] {
				continue
			}
			s.sendJSON(rcpt, Message{
				Type:    "massprivate",
				From:    client.nickname,
				Content: content,
			})
			delivered++
		}
		s.sendJSON(client, Message{
			Type:    "massprivatesent",
			Content: fmt.Sprintf("Отправлено приватно всем: %d", delivered),
		})
		// считаем как одно приватное отправление
		s.incPrivate()
	case "block":
		target := ""
		if msg.Data != nil {
			target = strings.TrimSpace(msg.Data["target"])
		}
		if target == "" || target == client.nickname {
			s.sendJSON(client, Message{Type: "error", Error: "Некорректный ник для блокировки"})
			return
		}
		client.blocked[target] = true
		s.sendJSON(client, Message{Type: "blocked", Content: fmt.Sprintf("Заблокирован: %s", target)})
	case "unblock":
		target := ""
		if msg.Data != nil {
			target = strings.TrimSpace(msg.Data["target"])
		}
		if target == "" {
			s.sendJSON(client, Message{Type: "error", Error: "Некорректный ник для разблокировки"})
			return
		}
		delete(client.blocked, target)
		s.sendJSON(client, Message{Type: "unblocked", Content: fmt.Sprintf("Разблокирован: %s", target)})
	case "fav":
		action := ""
		target := ""
		if msg.Data != nil {
			action = strings.ToLower(strings.TrimSpace(msg.Data["action"]))
			target = strings.TrimSpace(msg.Data["target"])
		}
		switch action {
		case "list":
			var list []string
			for u := range client.favoriteUsers {
				list = append(list, u)
			}
			s.sendJSON(client, Message{Type: "favlist", Content: strings.Join(list, ", ")})
		case "clear":
			client.favoriteUsers = make(map[string]bool)
			s.sendJSON(client, Message{Type: "favcleared", Content: "Избранные очищены"})
		case "add":
			if target == "" || target == client.nickname {
				s.sendJSON(client, Message{Type: "error", Error: "Некорректный ник для добавления"})
				return
			}
			client.favoriteUsers[target] = true
			s.sendJSON(client, Message{Type: "favadded", Content: fmt.Sprintf("Добавлен в избранные: %s", target)})
		case "remove":
			if target == "" {
				s.sendJSON(client, Message{Type: "error", Error: "Некорректный ник для удаления"})
				return
			}
			delete(client.favoriteUsers, target)
			s.sendJSON(client, Message{Type: "favremoved", Content: fmt.Sprintf("Удалён из избранных: %s", target)})
		default:
			s.sendJSON(client, Message{Type: "error", Error: "fav: допустимо list|clear|add|remove"})
		}
	default:
		s.sendJSON(client, Message{Type: "error", Error: "Неизвестная команда"})
	}
}

func (s *ChatServer) snapshotClients() []*Client {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	out := make([]*Client, 0, len(s.clients))
	for c := range s.clients {
		out = append(out, c)
	}
	return out
}

// --- main ---

func main() {
	host := getenvDefault("HOST", "0.0.0.0")
	port := atoiDefault(getenvDefault("PORT", "12345"), 12345)
	server := NewChatServer(host, port)
	if err := server.Start(); err != nil {
		log.Fatal(err)
	}
}

func getenvDefault(k, def string) string {
	v := os.Getenv(k)
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return def
	}
	return n
}
