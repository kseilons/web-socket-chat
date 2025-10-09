package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// JSON структуры для сообщений (должны совпадать с сервером)
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

type ChatClient struct {
	conn          *websocket.Conn
	nickname      string
	server        string
	port          int
	running       bool
	consoleReader *bufio.Reader
	done          chan struct{}
}

func NewChatClient(server string, port int) *ChatClient {
	return &ChatClient{
		server:        server,
		port:          port,
		running:       true,
		consoleReader: bufio.NewReader(os.Stdin),
		done:          make(chan struct{}),
	}
}

func (c *ChatClient) Connect() error {
	u := url.URL{Scheme: "ws", Host: fmt.Sprintf("%s:%d", c.server, c.port), Path: "/ws"}

	fmt.Printf("Подключение к %s...\n", u.String())

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return fmt.Errorf("не удалось подключиться к WebSocket серверу: %v", err)
	}

	c.conn = conn
	fmt.Printf("✅ Подключено к WebSocket серверу %s\n", u.String())
	return nil
}

// Функции для работы с WebSocket сообщениями
func (c *ChatClient) sendJSONMessage(msg Message) error {
	jsonData, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("ошибка сериализации JSON: %v", err)
	}

	return c.conn.WriteMessage(websocket.TextMessage, jsonData)
}

func (c *ChatClient) readJSONMessage() (*Message, error) {
	_, message, err := c.conn.ReadMessage()
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

func (c *ChatClient) Login() error {
	// Читаем первоначальный ответ от сервера
	initialMsg, err := c.readJSONMessage()
	if err != nil {
		return fmt.Errorf("ошибка получения приветствия от сервера: %v", err)
	}

	var nickname string

	if initialMsg.Type == "nick_prompt" {
		// Сервер предлагает предыдущий никнейм
		suggestedNick := initialMsg.Content

		fmt.Printf("🕒 Найден ваш предыдущий никнейм: %s\n", suggestedNick)
		fmt.Print("Нажмите Enter чтобы использовать его, или введите новый никнейм: ")

		input, err := c.consoleReader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("ошибка чтения ввода: %v", err)
		}

		input = strings.TrimSpace(input)
		if input == "" {
			nickname = suggestedNick
			fmt.Printf("✅ Используем предыдущий никнейм: %s\n", nickname)
		} else {
			nickname = input
		}
	} else if initialMsg.Type == "nick_request" {
		// Сервер запрашивает новый никнейм
		fmt.Print("Введите ваш никнейм: ")
		input, err := c.consoleReader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("ошибка чтения никнейма: %v", err)
		}
		nickname = strings.TrimSpace(input)
	} else {
		return fmt.Errorf("неожиданный ответ от сервера: %s", initialMsg.Type)
	}

	// Отправляем выбранный никнейм серверу
	c.nickname = nickname
	nickMsg := Message{
		Type:    "nick",
		Content: nickname,
	}
	err = c.sendJSONMessage(nickMsg)
	if err != nil {
		return fmt.Errorf("ошибка отправки никнейма: %v", err)
	}

	// Получаем подтверждение от сервера
	response, err := c.readJSONMessage()
	if err != nil {
		return fmt.Errorf("ошибка получения ответа от сервера: %v", err)
	}

	if response.Type == "error" {
		return fmt.Errorf("ошибка сервера: %s", response.Error)
	}

	if response.Type != "nick_ok" {
		return fmt.Errorf("неожиданный ответ от сервера: %s", response.Type)
	}

	fmt.Println("✅ Никнейм принят сервером")
	return nil
}

func (c *ChatClient) Start() {
	// Запускаем горутину для чтения сообщений
	go c.readMessages()

	fmt.Println("\n💬 Добро пожаловать в WebSocket чат!")
	fmt.Println("Доступные команды:")
	fmt.Println("  #help - показать справку")
	fmt.Println("  #users - список пользователей")
	fmt.Println("  #fav [ник] - добавить/удалить любимого писателя")
	fmt.Println("  #fav list - показать список любимых")
	fmt.Println("  #fav clear - очистить список")
	fmt.Println("  #all сообщение - массовое личное сообщение")
	fmt.Println("  @ник сообщение - приватное сообщение")
	fmt.Println("  #mailbox - проверить почтовый ящик")
	fmt.Println("  #last <ник> - показать последнее сообщение пользователя")
	fmt.Println("  #block ник - добавить в чёрный список")
	fmt.Println("  #unblock ник - убрать из чёрного списка")
	fmt.Println("  #color - установить случайный цвет текста сообщений")
	fmt.Println("  #color #hex - установить цвет текста сообщений (например, #FF0000)")
	fmt.Println("  #log - показать содержимое лог-файла сервера")
	fmt.Println("  #wordlengths - переключить режим показа длин слов")
	fmt.Println("  #UserZabor - включить/выключить режим «забор» для ваших сообщений")
	fmt.Println("  /quit - выход из чата")
	fmt.Println(strings.Repeat("=", 50))

	for c.running {
		fmt.Print("> ")
		message, err := c.consoleReader.ReadString('\n')
		if err != nil {
			fmt.Printf("❌ Ошибка чтения ввода: %v\n", err)
			continue
		}

		message = strings.TrimSpace(message)

		if message == "/quit" {
			fmt.Println("👋 Выход из чата...")
			c.running = false
			break
		}

		if message == "" {
			continue
		}

		// Обработка команд
		if strings.HasPrefix(message, "#") {
			c.handleCommand(message)
		} else if strings.HasPrefix(message, "@") {
			c.handlePrivateMessage(message)
		} else {
			// Обычное сообщение
			msg := Message{
				Type:    "message",
				Content: message,
			}
			err = c.sendJSONMessage(msg)
			if err != nil {
				fmt.Printf("❌ Ошибка отправки сообщения: %v\n", err)
				c.running = false
				break
			}
		}
	}

	c.cleanup()
}

func (c *ChatClient) handleCommand(message string) {
	parts := strings.SplitN(message, " ", 2)
	cmd := strings.ToLower(strings.TrimPrefix(parts[0], "#"))

	msg := Message{
		Type: "command",
		Data: make(map[string]string),
	}
	msg.Data["command"] = cmd

	switch cmd {
	case "help", "users", "mailbox", "wordlengths", "userzabor":
		// Простые команды без параметров
	case "last":
		if len(parts) < 2 {
			fmt.Println("❌ Использование: #last <ник>")
			return
		}
		msg.Data["target"] = parts[1]
	case "all":
		if len(parts) < 2 {
			fmt.Println("❌ Использование: #all сообщение")
			return
		}
		msg.Data["content"] = parts[1]
	case "block", "unblock":
		if len(parts) < 2 {
			fmt.Printf("❌ Использование: #%s ник\n", cmd)
			return
		}
		msg.Data["target"] = parts[1]
	case "color":
		if len(parts) >= 2 {
			msg.Data["target"] = parts[1]
		}
	case "fav":
		if len(parts) < 2 {
			msg.Data["action"] = "list"
		} else {
			subParts := strings.SplitN(parts[1], " ", 2)
			if len(subParts) == 1 {
				if strings.ToLower(subParts[0]) == "list" {
					msg.Data["action"] = "list"
				} else if strings.ToLower(subParts[0]) == "clear" {
					msg.Data["action"] = "clear"
				} else {
					msg.Data["action"] = "add"
					msg.Data["target"] = subParts[0]
				}
			} else {
				msg.Data["action"] = subParts[0]
				msg.Data["target"] = subParts[1]
			}
		}
	case "log":
		// Directly send the log command without redundant calls
	default:
		fmt.Printf("❌ Неизвестная команда: %s\n", cmd)
		return
	}

	err := c.sendJSONMessage(msg)
	if err != nil {
		fmt.Printf("❌ Ошибка отправки команды: %v\n", err)
	}
}

func (c *ChatClient) handlePrivateMessage(message string) {
	parts := strings.SplitN(message, " ", 2)
	if len(parts) < 2 {
		fmt.Println("❌ Использование: @никнейм сообщение")
		return
	}

	targetNick := strings.TrimPrefix(parts[0], "@")
	privateMsg := parts[1]

	msg := Message{
		Type:    "private",
		Content: privateMsg,
		To:      targetNick,
	}

	err := c.sendJSONMessage(msg)
	if err != nil {
		fmt.Printf("❌ Ошибка отправки личного сообщения: %v\n", err)
	}
}

func (c *ChatClient) requestLog() {
	msg := Message{
		Type: "command",
		Data: map[string]string{"command": "log"},
	}
	err := c.conn.WriteJSON(msg)
	if err != nil {
		fmt.Printf("❌ Ошибка запроса лог-файла: %v\n", err)
	}
}

func (c *ChatClient) readMessages() {
	for c.running {
		msg, err := c.readJSONMessage()
		if err != nil {
			if c.running {
				fmt.Printf("\n❌ Ошибка чтения сообщения: %v\n", err)
				fmt.Println("Сервер недоступен. Отключение...")
			}
			c.running = false
			break
		}

		c.handleServerMessage(msg)
	}
}

func (c *ChatClient) handleServerMessage(msg *Message) {
	switch msg.Type {
	case "chat":
		// Обычное сообщение в чат
		c.printChatMessage(msg)
	case "private":
		// Личное сообщение
		c.printPrivateMessage(msg)
	case "private_sent":
		// Подтверждение отправки личного сообщения - не показываем
		// Просто игнорируем это сообщение
	case "mass_private":
		// Массовое личное сообщение
		c.printMassPrivateMessage(msg)
	case "mass_private_sent":
		// Подтверждение отправки массового сообщения - не показываем
		// Просто игнорируем это сообщение
	case "system":
		// Системное сообщение
		c.printSystemMessage(msg)
	case "users":
		// Список пользователей
		c.handleUserList(msg)
	case "help":
		// Справка
		c.handleHelp(msg)
	case "mailbox_status":
		// Статус почтового ящика
		c.printMailboxStatus(msg)
	case "last_result":
		// Результат команды #last
		if msg.From != "" {
			fmt.Printf("\nПоследнее от %s (%s): %s\n> ", msg.From, msg.Timestamp, msg.Content)
		} else {
			fmt.Printf("\n%s\n> ", msg.Content)
		}
	case "offline_message":
		// Отложенное сообщение
		c.printOfflineMessage(msg)
	case "offline_delivered":
		// Уведомление о доставке отложенных сообщений
		c.printOfflineDelivered(msg)
	case "offline_saved":
		// Сообщение сохранено для оффлайн пользователя
		c.printOfflineSaved(msg)
	case "fav_list":
		// Список любимых писателей
		c.handleFavList(msg)
	case "fav_added":
		// Пользователь добавлен в любимые
		c.printFavAdded(msg)
	case "fav_removed":
		// Пользователь удален из любимых
		c.printFavRemoved(msg)
	case "fav_cleared":
		// Список любимых очищен
		c.printFavCleared(msg)
	case "blocked":
		// Пользователь заблокирован
		c.printBlocked(msg)
	case "unblocked":
		// Пользователь разблокирован
		c.printUnblocked(msg)
	case "color_set":
		// Цвет установлен
		c.printColorSet(msg)
	case "log":
		fmt.Println("\n📜 Содержимое лог-файла сервера:")
		fmt.Println(strings.Repeat("─", 60))
		fmt.Println(msg.Content)
		fmt.Println(strings.Repeat("─", 60))
	case "wordlengths_toggle":
		// Переключение режима показа длин слов
		c.printWordLengthsToggle(msg)
	case "error":
		// Ошибка
		c.printError(msg)
	default:
		fmt.Printf("❓ Неизвестный тип сообщения: %s\n", msg.Type)
	}
}

// Функции для обработки различных типов сообщений
func (c *ChatClient) printChatMessage(msg *Message) {
	// Получаем цвет отправителя
	color := ""
	if msg.Data != nil {
		color = msg.Data["color"]
	}

	// Проверяем флаги для определения типа сообщения
	if msg.Flags != nil && msg.Flags["favorite"] {
		// Сообщение от любимого писателя
		coloredContent := getColoredText(msg.Content, color)
		fmt.Printf("\n\033[1;33m✨ %s: %s\033[0m\n> ", msg.From, coloredContent)
	} else {
		// Обычное сообщение
		coloredContent := getColoredText(msg.Content, color)
		fmt.Printf("\n%s: %s\n> ", msg.From, coloredContent)
	}
}

func (c *ChatClient) printPrivateMessage(msg *Message) {
	// Получаем цвет отправителя
	color := ""
	if msg.Data != nil {
		color = msg.Data["color"]
	}

	// Проверяем флаги для определения типа сообщения
	if msg.Flags != nil && msg.Flags["favorite"] {
		// Сообщение от любимого писателя
		coloredContent := getColoredText(msg.Content, color)
		fmt.Printf("\n\033[1;33m✨ %s: %s\033[0m\n> ", msg.From, coloredContent)
	} else {
		// Обычное личное сообщение
		coloredContent := getColoredText(msg.Content, color)
		fmt.Printf("\n\033[36m%s: %s\033[0m\n> ", msg.From, coloredContent)
	}
}

func (c *ChatClient) printPrivateSentMessage(msg *Message) {
	fmt.Printf("\n\033[36mВы → %s: %s\033[0m\n> ", msg.To, msg.Content)
}

func (c *ChatClient) printMassPrivateMessage(msg *Message) {
	// Получаем цвет отправителя
	color := ""
	if msg.Data != nil {
		color = msg.Data["color"]
	}

	// Проверяем флаги для определения типа сообщения
	if msg.Flags != nil && msg.Flags["favorite"] {
		// Сообщение от любимого писателя
		coloredContent := getColoredText(msg.Content, color)
		fmt.Printf("\n\033[1;33m✨ %s: %s\033[0m\n> ", msg.From, coloredContent)
	} else {
		// Обычное массовое сообщение
		coloredContent := getColoredText(msg.Content, color)
		fmt.Printf("\n\033[35m%s: %s\033[0m\n> ", msg.From, coloredContent)
	}
}

func (c *ChatClient) printMassPrivateSentMessage(msg *Message) {
	fmt.Printf("\n\033[35mВы: %s\033[0m\n> ", msg.Content)
}

func (c *ChatClient) printSystemMessage(msg *Message) {
	fmt.Printf("\n%s\n> ", msg.Content)
}

func (c *ChatClient) printMailboxStatus(msg *Message) {
	fmt.Printf("\n📬 %s\n> ", msg.Content)
}

func (c *ChatClient) printOfflineMessage(msg *Message) {
	fmt.Printf("\n\033[33m[📮] %s (оффлайн): %s\033[0m\n> ", msg.From, msg.Content)
}

func (c *ChatClient) printOfflineDelivered(msg *Message) {
	fmt.Printf("\n📬 %s\n> ", msg.Content)
}

func (c *ChatClient) printOfflineSaved(msg *Message) {
	fmt.Printf("\n📮 %s\n> ", msg.Content)
}

func (c *ChatClient) printFavAdded(msg *Message) {
	fmt.Printf("\n❤️ %s\n> ", msg.Content)
}

func (c *ChatClient) printFavRemoved(msg *Message) {
	fmt.Printf("\n✅ %s\n> ", msg.Content)
}

func (c *ChatClient) printFavCleared(msg *Message) {
	fmt.Printf("\n✅ %s\n> ", msg.Content)
}

func (c *ChatClient) printBlocked(msg *Message) {
	fmt.Printf("\n🚫 %s\n> ", msg.Content)
}

func (c *ChatClient) printUnblocked(msg *Message) {
	fmt.Printf("\n✅ %s\n> ", msg.Content)
}

func (c *ChatClient) printColorSet(msg *Message) {
	fmt.Printf("\n🎨 %s\n> ", msg.Content)
}

func (c *ChatClient) printWordLengthsToggle(msg *Message) {
	fmt.Printf("\n🔢 %s\n> ", msg.Content)
}

func (c *ChatClient) printError(msg *Message) {
	fmt.Printf("\n❌ %s\n> ", msg.Error)
}

// hexToANSI converts a hex color to ANSI color code
func hexToANSI(hex string) string {
	if hex == "" || len(hex) != 7 || hex[0] != '#' {
		return ""
	}

	// Parse hex values
	r, err1 := strconv.ParseInt(hex[1:3], 16, 64)
	g, err2 := strconv.ParseInt(hex[3:5], 16, 64)
	b, err3 := strconv.ParseInt(hex[5:7], 16, 64)

	if err1 != nil || err2 != nil || err3 != nil {
		return ""
	}

	// Convert to ANSI 256 color (approximation)
	ansi := 16 + (r/51)*36 + (g/51)*6 + (b / 51)
	return fmt.Sprintf("\033[38;5;%dm", ansi)
}

// getColoredText returns text with color if color is provided
func getColoredText(text, color string) string {
	if color == "" {
		return text
	}
	ansiColor := hexToANSI(color)
	if ansiColor == "" {
		return text
	}
	return ansiColor + text + "\033[0m"
}

func (c *ChatClient) handleUserList(msg *Message) {
	fmt.Printf("\n👥 Пользователи онлайн (%d):\n", len(msg.Users))
	for _, user := range msg.Users {
		if user != "" {
			status := "🟢"
			if user == c.nickname {
				status = "🟡 (вы)"
			}
			fmt.Printf("%s %s\n", status, user)
		}
	}
	fmt.Print("> ")
}

func (c *ChatClient) handleHelp(msg *Message) {
	fmt.Printf("\n\033[1;34m📖 Справка по командам чата:\033[0m\n")
	fmt.Println(strings.Repeat("─", 60))

	for cmd, desc := range msg.Data {
		fmt.Printf("\033[1;32m%-25s\033[0m %s\n", cmd, desc)
	}

	fmt.Println(strings.Repeat("─", 60))
	fmt.Print("> ")
}

func (c *ChatClient) handleFavList(msg *Message) {
	if len(msg.Users) == 0 {
		fmt.Printf("\n📝 %s\n> ", msg.Content)
	} else {
		fmt.Printf("\n❤️ %s\n> ", msg.Content)
	}
}

func (c *ChatClient) cleanup() {
	if c.conn != nil {
		// Отправляем сообщение о закрытии соединения
		c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		time.Sleep(time.Second) // Даем время на отправку сообщения
		c.conn.Close()
	}
	fmt.Println("✅ WebSocket соединение закрыто")
}

func (c *ChatClient) WaitForInterrupt() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	<-sigChan
	fmt.Println("\n🛑 Получен сигнал прерывания...")
	c.running = false
	c.cleanup()
	os.Exit(0)
}

func getServerAddress() (string, int) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("=== 💬 Go клиент для чат-сервера ===")
	fmt.Println("Введите адрес сервера (по умолчанию: localhost:12345)")
	fmt.Print("Адрес сервера [localhost]: ")

	serverInput, _ := reader.ReadString('\n')
	serverInput = strings.TrimSpace(serverInput)

	if serverInput == "" {
		return "localhost", 12345
	}

	if strings.Contains(serverInput, ":") {
		parts := strings.Split(serverInput, ":")
		if len(parts) == 2 {
			server := parts[0]
			var port int
			if _, err := fmt.Sscanf(parts[1], "%d", &port); err == nil && port > 0 && port < 65536 {
				return server, port
			}
		}
	}

	return serverInput, 12345
}

func main() {
	server, port := getServerAddress()

	fmt.Printf("Подключение к %s:%d...\n", server, port)

	client := NewChatClient(server, port)
	go client.WaitForInterrupt()

	err := client.Connect()
	if err != nil {
		fmt.Printf("❌ Ошибка подключения: %v\n", err)
		os.Exit(1)
	}
	defer client.cleanup()

	err = client.Login()
	if err != nil {
		fmt.Printf("❌ Ошибка входа: %v\n", err)
		os.Exit(1)
	}

	client.Start()
}
