package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"syscall"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"
	"github.com/streadway/amqp"
)

const (
	receiveMessageQueueName = "q.message.receive"
	sendMessageQueueName    = "q.message.send"
)

var (
	amqpHost     = os.Getenv("AMQP_HOST")
	amqpPort     = os.Getenv("AMQP_PORT")
	amqpUser     = os.Getenv("AMQP_USER")
	amqpPassword = os.Getenv("AMQP_PASSWORD")

	rabbitMQURL     = "amqp://" + amqpUser + ":" + amqpPassword + "@" + amqpHost + ":" + amqpPort + "/"
	rabbitMQChannel *amqp.Channel
)

func failOnErrorWithTransaction(err error, transactionId string) {
	if err != nil {
		logWithTransaction(transactionId, "ERROR", err.Error())
		panic(err)
	}
}

func logWithTransaction(transactionId string, level string, message string) {
	log.Println(level + ": " + transactionId + " - " + message)
}

func failOnError(err error, msg string) {
	if err != nil {
		log.Fatalf("%s: %s", msg, err)
	}
}

type SendMessagePayload struct {
	MessageType     string `json:"message_type"`
	QuotedMessageID string `json:"quoted_message_id"`
	RecipientNumber string `json:"recipient_number"`
	MessageBody     string `json:"message_body"`
	TransactionId   string `json:"transaction_id"`
}

type ReceiveMessagePayload struct {
	MessageType     string `json:"message_type"`
	MessageId       string `json:"message_id"`
	SenderNumber    string `json:"sender_number"`
	MessageBody     string `json:"message_body"`
	TransactionId   string `json:"transaction_id"`
	QuotedMessageID string `json:"quoted_message_id"`
}

func NewReceiveMessagePayload() *ReceiveMessagePayload {
	return &ReceiveMessagePayload{
		TransactionId: uuid.New().String(),
	}
}

func NewSendMessagePayload() *SendMessagePayload {
	return &SendMessagePayload{
		TransactionId: uuid.New().String(),
	}
}

func setUpRabbitMQConsumer(channel *amqp.Channel) <-chan amqp.Delivery {
	messages, err := channel.Consume(
		sendMessageQueueName,
		"",
		false,
		false,
		false,
		false,
		nil,
	)
	failOnError(err, "Error starting consumer")

	return messages
}

func handleSendWhatsappMessage(client *whatsmeow.Client, body []byte) {
	payload := NewSendMessagePayload()
	err := json.Unmarshal(body, &payload)
	failOnErrorWithTransaction(err, payload.TransactionId)

	recipientJID := types.NewJID(payload.RecipientNumber, types.DefaultUserServer)

	recipientJIDString := recipientJID.String()

	msg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(payload.MessageBody),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID: proto.String(payload.QuotedMessageID),
				Participant: &recipientJIDString,
			},
		},
	}

	logWithTransaction(payload.TransactionId, "INFO", "Sending message")

	_, err = client.SendMessage(context.Background(), recipientJID, msg)

	failOnErrorWithTransaction(err, payload.TransactionId)
}

func initRabbitMQ() *amqp.Channel {
	connection, err := amqp.Dial(rabbitMQURL)
	failOnError(err, "Could not connect to RabbitMQ host")

	channel, err := connection.Channel()
	failOnError(err, "Could not create RabbitMQ channel")

	return channel
}

func messageReceiveHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		switch v.Info.Type {
		case "text":
			payload := NewReceiveMessagePayload()
			payload.MessageType = "text"
			payload.SenderNumber = v.Info.Sender.User
			payload.MessageId = v.Info.ID

			if v.Message.Conversation != nil {
				payload.MessageBody = *v.Message.Conversation
			} else if v.Message.ExtendedTextMessage != nil {
				payload.MessageBody = v.Message.ExtendedTextMessage.GetText()
				if v.Message.ExtendedTextMessage.ContextInfo != nil {
				payload.QuotedMessageID = *v.Message.ExtendedTextMessage.ContextInfo.StanzaID
			}
			} else {
				payload.MessageBody = "[Unsupported message type]"
			}

			logWithTransaction(payload.TransactionId, "INFO", "Received message from "+payload.SenderNumber)

			msgData, err := json.Marshal(payload)
			failOnErrorWithTransaction(err, payload.TransactionId)

			err = rabbitMQChannel.Publish(
				"",
				receiveMessageQueueName,
				false,
				false,
				amqp.Publishing{
					ContentType: "application/json",
					Body:        msgData,
				},
			)
			failOnErrorWithTransaction(err, payload.TransactionId)
		}
	}
}

func main() {
	dbLog := waLog.Stdout("Database", "DEBUG", true)

	container, err := sqlstore.New("sqlite3", "file:storage/wadb.db?_foreign_keys=on", dbLog)
	failOnError(err, "Error creating db container")

	deviceStore, err := container.GetFirstDevice()
	failOnError(err, "Error getting device")

	clientLog := waLog.Stdout("Client", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)
	client.AddEventHandler(messageReceiveHandler)

	rabbitMQChannel = initRabbitMQ()

	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		failOnError(err, "Error connecting to client")

		for evt := range qrChan {
			if evt.Event == "code" {
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			}
		}
	} else {
		err = client.Connect()
		failOnError(err, "Error connecting to client")
	}

	messages := setUpRabbitMQConsumer(rabbitMQChannel)

	go func() {
		for message := range messages {
			handleSendWhatsappMessage(client, message.Body)
			message.Ack(false)
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
}
