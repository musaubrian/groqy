package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
	"github.com/oklog/ulid/v2"
	tele "gopkg.in/telebot.v3"
)

const MODEL = "llama-3.1-8b-instant"

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type RequestBody struct {
	Messages    []Message `json:"messages"`
	Model       string    `json:"model"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens"`
	TopP        float64   `json:"top_p"`
	Stream      bool      `json:"stream"`
	Stop        *string   `json:"stop"`
}

type Command struct {
	Name        string
	Description string
	Handler     func(c tele.Context) error
}

type User struct {
	ID       string `db:"id"`
	Username string `db:"username"`
	Token    string `db:"token"`
}

type DB struct {
	db *sqlx.DB
}

func main() {
	slog.Info("Bot started")
	err := godotenv.Load()
	if err != nil {
		slog.Error("Error loading .env file")
		return
	}
	botToken := os.Getenv("BOT_TOKEN")

	db, err := connectToDB()
	if err != nil {
		slog.Error(fmt.Sprintf("Could not conect to db:\n%v", err))
	}

	if err := db.CreateTables(); err != nil {
		slog.Error(fmt.Sprintf("Could not create tables:\n%v", err))
	}

	pref := tele.Settings{
		Token: botToken,
		Poller: &tele.LongPoller{
			Timeout:        2 * time.Second,
			AllowedUpdates: []string{"message"},
		},
		ParseMode: tele.ModeDefault,
	}

	b, err := tele.NewBot(pref)
	if err != nil {
		log.Fatal(err)
		return
	}

	commands := []Command{
		{Name: "/auth", Description: "Provide token to allow usage", Handler: func(c tele.Context) error {
			return authHandler(c, db)
		}},
	}

	// menu := createMenu(commands)

	b.Handle("/start", func(c tele.Context) error {
		return c.Send(fmt.Sprintf("Hello, %s", c.Sender().FirstName))
	})

	for _, cmd := range commands {
		b.Handle(cmd.Name, cmd.Handler)
	}

	b.Handle(tele.OnText, withAuth(db, func(c tele.Context) error {

		user, err := db.GetUser(c.Sender().Username)
		if err != nil {
			return err
		}
		if user.Username == "" {
			return c.Send("Can't seem to find you " + c.Sender().Username)
		}

		return chatHandler(c, c.Text())
	}))

	b.Start()
}

// func createMenu(commands []Command) *tele.ReplyMarkup {
// 	menu := &tele.ReplyMarkup{ResizeKeyboard: true}
// 	buttons := []tele.Row{}
// 	for _, cmd := range commands {
// 		btn := menu.Text(cmd.Name)
// 		buttons = append(buttons, menu.Row(btn))
// 	}
// 	menu.Reply(buttons...)
// 	return menu
// }

func authHandler(c tele.Context, db *DB) error {
	user := c.Sender().Username
	args := c.Args()
	if len(args) != 1 {
		return c.Send("Either provided too many or too little arguments")
	}
	token := args[0]
	if !validateToken(token) {
		return c.Send("Invalid token")
	}
	if err := db.CreateUser(user, token); err != nil {
		return c.Send("ERROR: Could not save your token" + err.Error())
	}
	return c.Send("Authenticated successfully")
}

func chatHandler(tc tele.Context, userMessage string) error {
	var AIResponse string

	baseInstruct := "Do not use any markdown formatting in your response, keep it plain text\n\n\n"
	res, err := queryGroq(baseInstruct + userMessage)
	if err != nil {
		slog.Error(err.Error())
		AIResponse = "An error occured"
	}
	AIResponse = res

	return tc.Send(AIResponse)
}

func connectToDB() (*DB, error) {
	db, err := sqlx.Open("sqlite3", "./sqlite.db")
	if err != nil {
		return nil, err
	}
	return &DB{db: db}, nil
}

func (d *DB) CreateTables() error {
	schema := `
CREATE TABLE IF NOT EXISTS users (
	id TEXT NOT NULL PRIMARY KEY,
	username TEXT NOT NULL,
	token TEXT NOT NULL
);
    `
	_, err := d.db.Exec(schema)
	return err

}

func (d *DB) CreateUser(username, token string) error {
	id := ulid.Make().String()
	_, err := d.db.Exec("INSERT INTO users(id, username, token) VALUES(?, ?, ?)", id, username, token)
	return err
}

func (d *DB) GetUser(username string) (User, error) {
	var user User
	err := d.db.Get(&user, "SELECT * FROM users WHERE username=?", username)
	if err != nil {
		if err == sql.ErrNoRows {
			return user, fmt.Errorf("user not found")
		}
		return user, err
	}
	return user, err
}

func (d *DB) Cleanup() {
	d.db.MustExec("DROP TABLE users")
}

func validateToken(token string) bool {
	expectedToken := os.Getenv("AUTH_TOKEN")
	return token == expectedToken
}

func checkAuth(c tele.Context, db *DB) error {
	user := c.Sender().Username

	dbUser, err := db.GetUser(user)
	if err != nil {
		return fmt.Errorf("could not get user: %v", err)
	}

	if validateToken(dbUser.Token) {
		return nil
	}

	return fmt.Errorf("invalid token")
}

func withAuth(db *DB, handler func(c tele.Context) error) func(c tele.Context) error {
	return func(c tele.Context) error {
		if err := checkAuth(c, db); err != nil {
			return c.Send("Authentication required\nPlease use /auth yourtoken")
		}
		return handler(c)
	}
}

func queryGroq(message string) (string, error) {
	apiKey := os.Getenv("GROQ_TOKEN")

	url := "https://api.groq.com/openai/v1/chat/completions"

	requestBody := RequestBody{
		Messages: []Message{
			{
				Role:    "user",
				Content: message,
			},
		},
		Model:       MODEL,
		Temperature: 0.5,
		MaxTokens:   1024,
		TopP:        1,
		Stream:      false,
		Stop:        nil,
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("Error marshaling JSON:\n%v", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", fmt.Errorf("Error creating request:\n%v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Error sending request:\n%v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("Error reading response body:\n%v", err)
	}

	var responseBody struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	err = json.Unmarshal(body, &responseBody)
	if err != nil {
		return "", fmt.Errorf("Error unmarshaling response: %v", err)
	}

	if len(responseBody.Choices) == 0 {
		return "", fmt.Errorf("No message found in the response")
	}

	return responseBody.Choices[0].Message.Content, nil
}
