package consumer

import (
	"encoding/json"
	"fmt"
	"github.com/AdOnWeb/postmanq/common"
	"github.com/AdOnWeb/postmanq/logger"
	"github.com/streadway/amqp"
	"regexp"
	"sync"
)

var (
	resultHandlers = map[common.SendEventResult]func(*Consumer, *amqp.Channel, *common.MailMessage){
		common.ErrorSendEventResult:     (*Consumer).handleErrorSend,
		common.DelaySendEventResult:     (*Consumer).handleDelaySend,
		common.OverlimitSendEventResult: (*Consumer).handleOverlimitSend,
	}
)

// получатель сообщений из очереди
type Consumer struct {
	id         int
	connect    *amqp.Connection
	binding    *Binding
	deliveries <-chan amqp.Delivery
}

// создает нового получателя
func NewConsumer(id int, connect *amqp.Connection, binding *Binding) *Consumer {
	app := new(Consumer)
	app.id = id
	app.connect = connect
	app.binding = binding
	return app
}

// запускает получение сообщений из очереди в заданное количество потоков
func (c *Consumer) run() {
	for i := 0; i < c.binding.Handlers; i++ {
		go c.consume(i)
	}
}

// получает сообщения из очереди
func (c *Consumer) consume(id int) {
	channel, err := c.connect.Channel()
	// выбираем из очереди сообщения с запасом
	// это нужно для того, чтобы после отправки письма новое уже было готово к отправке
	// в тоже время нельзя выбираеть все сообщения из очереди разом, т.к. можно упереться в память
	channel.Qos(c.binding.PrefetchCount, 0, false)
	deliveries, err := channel.Consume(
		c.binding.Queue, // name
		"",              // consumerTag,
		false,           // noAck
		false,           // exclusive
		false,           // noLocal
		false,           // noWait
		nil,             // arguments
	)
	if err == nil {
		go c.consumeDeliveries(id, channel, deliveries)
	} else {
		logger.Warn("consumer#%d, handler#%d can't consume queue %s", c.id, id, c.binding.Queue)
	}
}

func (c *Consumer) consumeDeliveries(id int, channel *amqp.Channel, deliveries <-chan amqp.Delivery) {
	for delivery := range deliveries {
		message := new(common.MailMessage)
		err := json.Unmarshal(delivery.Body, message)
		if err == nil {
			// инициализируем параметры письма
			message.Init()
			logger.Info(
				"consumer#%d, handler#%d send mail#%d: envelope - %s, recipient - %s to mailer",
				c.id,
				id,
				message.Id,
				message.Envelope,
				message.Recipient,
			)

			event := common.NewSendEvent(message)
			logger.Debug("consumer#%d create send event for message#%d", c.id, message.Id)
			event.Iterator.Next().(common.SendingService).Events() <- event
			// ждем результата,
			// во время ожидания поток блокируется
			// если этого не сделать, тогда невозможно будет подтвердить получение сообщения из очереди
			if handler, ok := resultHandlers[<-event.Result]; ok {
				handler(c, channel, message)
			}
			message = nil
			event = nil
		} else {
			failureBinding := c.binding.failureBindings[TechnicalFailureBindingType]
			err = channel.Publish(
				failureBinding.Exchange,
				failureBinding.Routing,
				false,
				false,
				amqp.Publishing{
					ContentType:  "text/plain",
					Body:         delivery.Body,
					DeliveryMode: amqp.Transient,
				},
			)
			logger.Warn("can't unmarshal delivery body, body should be json, body is %s", string(delivery.Body))
		}
		// всегда подтверждаем получение сообщения
		// даже если во время отправки письма возникли ошибки,
		// мы уже положили это письмо в другую очередь
		delivery.Ack(true)
	}
}

func (c *Consumer) handleErrorSend(channel *amqp.Channel, message *common.MailMessage) {
	// если есть ошибка при отправке, значит мы попали в серый список https://ru.wikipedia.org/wiki/%D0%A1%D0%B5%D1%80%D1%8B%D0%B9_%D1%81%D0%BF%D0%B8%D1%81%D0%BE%D0%BA
	// или получили какую то ошибку от почтового сервиса, что он не может
	// отправить письмо указанному адресату или выполнить какую то команду
	var failureBinding *Binding
	// если ошибка связана с невозможностью отправить письмо адресату
	// перекладываем письмо в очередь для плохих писем
	// и пусть отправители сами с ними разбираются
	if message.Error.Code >= 500 && message.Error.Code < 600 {
		failureBinding = c.binding.failureBindings[errorSignsMap.BindingType(message)]
	} else if message.Error.Code == 451 { // мы точно попали в серый список, надо повторить отправку письма попозже
		failureBinding = delayedBindings[common.ThirtyMinutesDelayedBinding]
	} else {
		failureBinding = c.binding.failureBindings[UnknownFailureBindingType]
	}
	jsonMessage, err := json.Marshal(message)
	if err == nil {
		// кладем в очередь
		err = channel.Publish(
			failureBinding.Exchange,
			failureBinding.Routing,
			false,
			false,
			amqp.Publishing{
				ContentType:  "text/plain",
				Body:         jsonMessage,
				DeliveryMode: amqp.Transient,
			},
		)
		if err == nil {
			logger.Debug(
				"reason is %s with code %d, publish failure mail#%d to queue %s",
				message.Error.Message,
				message.Error.Code,
				message.Id,
				failureBinding.Queue,
			)
		} else {
			logger.Debug(
				"can't publish failure mail#%d with error %s and code %d to queue %s",
				message.Id,
				message.Error.Message,
				message.Error.Code,
				failureBinding.Queue,
			)
			logger.WarnWithErr(err)
		}
	} else {
		logger.WarnWithErr(err)
	}
}

func (c *Consumer) handleDelaySend(channel *amqp.Channel, message *common.MailMessage) {
	bindingType := common.UnknownDelayedBinding
	if message.Error != nil {
		logger.Debug(
			"reason is %s with code %d, find dlx queue for mail#%d",
			message.Error.Message,
			message.Error.Code,
			message.Id,
		)
	}
	logger.Debug("old dlx queue type %d for mail#%d", message.BindingType, message.Id)
	// если нам просто не удалось отправить письмо, берем следующую очередь из цепочки
	if chainBinding, ok := bindingsChain[message.BindingType]; ok {
		bindingType = chainBinding
	}
	c.publishDelayedMessage(channel, bindingType, message)
}

func (c *Consumer) handleOverlimitSend(channel *amqp.Channel, message *common.MailMessage) {
	bindingType := common.UnknownDelayedBinding
	logger.Debug("reason is overlimit, find dlx queue for mail#%d", message.Id)
	for i := 0; i < limitBindingsLen; i++ {
		if limitBindings[i] == message.BindingType {
			bindingType = limitBindings[i]
			break
		}
	}
	c.publishDelayedMessage(channel, bindingType, message)
}

func (c *Consumer) publishDelayedMessage(channel *amqp.Channel, bindingType common.DelayedBindingType, message *common.MailMessage) {
	logger.Debug("new dlx queue type %d for mail#%d", bindingType, message.Id)

	// получаем очередь, проверяем, что она реально есть
	// а что? а вдруг нет)
	if delayedBinding, ok := c.binding.delayedBindings[bindingType]; ok {
		message.BindingType = bindingType
		jsonMessage, err := json.Marshal(message)
		if err == nil {
			// кладем в очередь
			err = channel.Publish(
				delayedBinding.Exchange,
				delayedBinding.Routing,
				false,
				false,
				amqp.Publishing{
					ContentType:  "text/plain",
					Body:         []byte(jsonMessage),
					DeliveryMode: amqp.Transient,
				},
			)
			if err == nil {
				logger.Debug("publish failure mail#%d to queue %s", message.Id, delayedBinding.Queue)
			} else {
				logger.Warn("can't publish failure mail#%d to queue %s, error - %v", message.Id, delayedBinding.Queue, err)
			}
		} else {
			logger.Warn("can't marshal mail#%d to json", message.Id)
		}
	} else {
		logger.Warn("unknow delayed type %v for mail#%d", bindingType, message.Id)
	}
}

func (c *Consumer) consumeFailureMessages(group *sync.WaitGroup) {
	channel, err := c.connect.Channel()
	if err == nil {
		for _, failureBinding := range c.binding.failureBindings {
			for {
				delivery, ok, _ := channel.Get(failureBinding.Queue, false)
				if ok {
					message := new(common.MailMessage)
					err = json.Unmarshal(delivery.Body, message)
					if err == nil {
						sendEvent := common.NewSendEvent(message)
						sendEvent.Iterator.Next().(common.ReportService).Events() <- sendEvent
					}
				} else {
					break
				}
			}
		}
		group.Done()
	} else {
		logger.WarnWithErr(err)
	}
}

func (c *Consumer) consumeAndPublishMessages(event *common.ApplicationEvent, group *sync.WaitGroup) {
	channel, err := c.connect.Channel()
	if err == nil {
		var envelopeRegex, recipientRegex *regexp.Regexp
		srcBinding := c.findBindingByQueueName(event.GetStringArg("srcQueue"))
		if srcBinding == nil {
			fmt.Println("source queue should be defined")
			common.App.Events() <- common.NewApplicationEvent(common.FinishApplicationEventKind)
		}
		destBinding := c.findBindingByQueueName(event.GetStringArg("destQueue"))
		if destBinding == nil {
			fmt.Println("destination queue should be defined")
			common.App.Events() <- common.NewApplicationEvent(common.FinishApplicationEventKind)
		}
		if srcBinding == destBinding {
			fmt.Println("source and destination queue should be different")
			common.App.Events() <- common.NewApplicationEvent(common.FinishApplicationEventKind)
		}
		if len(event.GetStringArg("envelope")) > 0 {
			envelopeRegex, _ = regexp.Compile(event.GetStringArg("envelope"))
		}
		if len(event.GetStringArg("recipient")) > 0 {
			recipientRegex, _ = regexp.Compile(event.GetStringArg("recipient"))
		}

		publishDeliveries := make([]amqp.Delivery, 0)
		for {
			delivery, ok, _ := channel.Get(srcBinding.Queue, false)
			if ok {
				message := new(common.MailMessage)
				err = json.Unmarshal(delivery.Body, message)
				if err == nil {
					var necessaryPublish bool
					if (event.GetIntArg("code") > common.InvalidInputInt && event.GetIntArg("code") == message.Error.Code) ||
						(envelopeRegex != nil && envelopeRegex.MatchString(message.Envelope)) ||
						(recipientRegex != nil && recipientRegex.MatchString(message.Recipient)) ||
						(event.GetIntArg("code") == common.InvalidInputInt && envelopeRegex == nil && recipientRegex == nil) {
						necessaryPublish = true
					}
					if necessaryPublish {
						fmt.Printf(
							"find mail#%d: envelope - %s, recipient - %s\n",
							message.Id,
							message.Envelope,
							message.Recipient,
						)
						publishDeliveries = append(publishDeliveries, delivery)
					}
				}
			} else {
				break
			}
		}

		for _, delivery := range publishDeliveries {
			err = channel.Publish(
				destBinding.Exchange,
				destBinding.Routing,
				false,
				false,
				amqp.Publishing{
					ContentType:  "text/plain",
					Body:         delivery.Body,
					DeliveryMode: amqp.Transient,
				},
			)
			if err == nil {
				delivery.Ack(true)
			} else {
				delivery.Nack(true, true)
			}
		}
		group.Done()
	} else {
		logger.WarnWithErr(err)
	}
}

func (c *Consumer) findBindingByQueueName(queueName string) *Binding {
	var binding *Binding

	if c.binding.Queue == queueName {
		binding = c.binding
	}

	if binding == nil {
		for _, failureBinding := range c.binding.failureBindings {
			if failureBinding.Queue == queueName {
				binding = failureBinding
				break
			}
		}
	}

	if binding == nil {
		for _, delayedBinding := range c.binding.delayedBindings {
			if delayedBinding.Queue == queueName {
				binding = delayedBinding
				break
			}
		}
	}

	return binding
}
