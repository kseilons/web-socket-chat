package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

type ChatClient struct {
	conn          net.Conn
	nickname      string
	server        string
	port          int
	running       bool
	reader        *bufio.Reader
	writer        *bufio.Writer
	favoriteUser  string
	consoleReader *bufio.Reader
	blocked       map[string]bool // локальный чёрный список
}

func NewChatClient(server string, port int) *ChatClient {
	return &ChatClient{
		server:        server,
		port:          port,
		running:       true,
		consoleReader: bufio.NewReader(os.Stdin),
		blocked:       make(map[string]bool),
	}
}

func (c *ChatClient) Connect() error {
	address := fmt.Sprintf("%s:%d", c.server, c.port)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		return fmt.Errorf("не удалось подключиться к серверу: %v", err)
	}

	c.conn = conn
	c.reader = bufio.NewReader(conn)
	c.writer = bufio.NewWriter(conn)

	fmt.Printf("✅ Подключено к серверу %s\n", address)
	return nil
}

func (c *ChatClient) Login() error {
	fmt.Print("Введите ваш никнейм: ")
	nickname, err := c.consoleReader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("ошибка чтения никнейма: %v", err)
	}

	nickname = strings.TrimSpace(nickname)
	c.nickname = nickname

	nickMsg := fmt.Sprintf("NICK:%s\n", nickname)
	_, err = c.writer.WriteString(nickMsg)
	if err != nil {
		return fmt.Errorf("ошибка отправки никнейма: %v", err)
	}
	c.writer.Flush()

	response, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("ошибка получения ответа от сервера: %v", err)
	}

	response = strings.TrimSpace(response)
	if response == "NICK_TAKEN" {
		return fmt.Errorf("никнейм '%s' уже занят", nickname)
	}

	if response != "NICK_OK" {
		return fmt.Errorf("неожиданный ответ от сервера: %s", response)
	}

	fmt.Println("✅ Никнейм принят сервером")
	return nil
}

func (c *ChatClient) Start() {
	go c.readMessages()

	fmt.Println("\n💬 Добро пожаловать в чат!")
	fmt.Println("Доступные команды:")
	fmt.Println("  #help - показать справку")
	fmt.Println("  #users - список пользователей")
	fmt.Println("  #all сообщение - массовое личное сообщение")
	fmt.Println("  @ник сообщение - приватное сообщение")
	fmt.Println("  #block ник - добавить в чёрный список")
	fmt.Println("  #unblock ник - убрать из чёрного списка")
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

		_, err = c.writer.WriteString(message + "\n")
		if err != nil {
			fmt.Printf("❌ Ошибка отправки сообщения: %v\n", err)
			c.running = false
			break
		}
		c.writer.Flush()
	}

	c.cleanup()
}

func (c *ChatClient) readMessages() {
	for c.running {
		message, err := c.reader.ReadString('\n')
		if err != nil {
			if c.running {
				fmt.Printf("\n❌ Ошибка чтения сообщения: %v\n", err)
				fmt.Println("Сервер недоступен. Отключение...")
			}
			c.running = false
			break
		}

		message = strings.TrimSpace(message)

		// Обработка спец сообщений
		if strings.HasPrefix(message, "USERS:") {
			c.handleUserList(message)
			continue
		}

		// Проверка на блокировку
		for blockedUser := range c.blocked {
			if strings.Contains(message, blockedUser) {
				// пропускаем вывод
				continue
			}
		}

		// Подсветка типов сообщений
		if strings.HasPrefix(message, "[ЛС]") {
			fmt.Printf("\n\033[36m%s\033[0m\n> ", message) // голубой
		} else if strings.HasPrefix(message, "[МЛС]") {
			fmt.Printf("\n\033[35m%s\033[0m\n> ", message) // фиолетовый
		} else {
			fmt.Printf("\n%s\n> ", message) // обычное сообщение
		}
	}
}

func (c *ChatClient) handleUserList(message string) {
	users := strings.TrimPrefix(message, "USERS:")
	userList := strings.Split(users, ",")

	fmt.Printf("\n👥 Пользователи онлайн (%d):\n", len(userList))
	for _, user := range userList {
		if user != "" {
			status := "🟢"
			if user == c.nickname {
				status = "🟡 (вы)"
			} else if user == c.favoriteUser && c.favoriteUser != "" {
				status = "❤️"
			}
			fmt.Printf("%s %s\n", status, user)
		}
	}
	fmt.Print("> ")
}

func (c *ChatClient) cleanup() {
	if c.conn != nil {
		c.conn.Close()
	}
	fmt.Println("✅ Соединение закрыто")
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
