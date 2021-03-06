package nats

import (
	"context"
	"sync"
	"time"

	internalSync "github.com/ThreeDotsLabs/watermill/internal/sync"

	"github.com/ThreeDotsLabs/watermill"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/nats-io/go-nats-streaming"
	"github.com/pkg/errors"
)

type StreamingSubscriberConfig struct {
	// ClusterID is the NATS Streaming cluster ID.
	ClusterID string

	// ClientID is the NATS Streaming client ID to connect with.
	// ClientID can contain only alphanumeric and `-` or `_` characters.
	//
	// Using DurableName causes the NATS Streaming server to track
	// the last acknowledged message for that ClientID + DurableName.
	ClientID string

	// QueueGroup is the NATS Streaming queue group.
	//
	// All subscriptions with the same queue name (regardless of the connection they originate from)
	// will form a queue group. Each message will be delivered to only one subscriber per queue group,
	// using queuing semantics.
	//
	// It is recommended to set it with DurableName.
	// For non durable queue subscribers, when the last member leaves the group,
	// that group is removed. A durable queue group (DurableName) allows you to have all members leave
	// but still maintain state. When a member re-joins, it starts at the last position in that group.
	//
	// When QueueGroup is empty, subscribe without QueueGroup will be used.
	QueueGroup string

	// DurableName is the NATS streaming durable name.
	//
	// Subscriptions may also specify a “durable name” which will survive client restarts.
	// Durable subscriptions cause the server to track the last acknowledged message
	// sequence number for a client and durable name. When the client restarts/resubscribes,
	// and uses the same client ID and durable name, the server will resume delivery beginning
	// with the earliest unacknowledged message for this durable subscription.
	//
	// Doing this causes the NATS Streaming server to track
	// the last acknowledged message for that ClientID + DurableName.
	DurableName string

	// SubscribersCount determines wow much concurrent subscribers should be started.
	SubscribersCount int

	// CloseTimeout determines how long subscriber will wait for Ack/Nack on close.
	// When no Ack/Nack is received after CloseTimeout, subscriber will be closed.
	CloseTimeout time.Duration

	// How long subscriber should wait for Ack/Nack. When no Ack/Nack was received, message will be redelivered.
	// It is mapped to stan.AckWait option.
	AckWaitTimeout time.Duration

	// StanOptions are custom []stan.Option passed to the connection.
	// It is also used to provide connection parameters, for example:
	// 		stan.NatsURL("nats://localhost:4222")
	StanOptions []stan.Option

	// StanSubscriptionOptions are custom []stan.SubscriptionOption passed to subscription.
	StanSubscriptionOptions []stan.SubscriptionOption

	// Unmarshaler is an unmarshaler used to unmarshaling messages from NATS format to Watermill format.
	Unmarshaler Unmarshaler
}

func (c *StreamingSubscriberConfig) setDefaults() {
	if c.SubscribersCount <= 0 {
		c.SubscribersCount = 1
	}
	if c.CloseTimeout <= 0 {
		c.CloseTimeout = time.Second * 30
	}
	if c.AckWaitTimeout <= 0 {
		c.AckWaitTimeout = time.Second * 30
	}

	c.StanSubscriptionOptions = append(
		c.StanSubscriptionOptions,
		stan.SetManualAckMode(), // manual AckMode is required to support acking/nacking by client
		stan.AckWait(c.AckWaitTimeout),
	)

	if c.DurableName != "" {
		c.StanSubscriptionOptions = append(c.StanSubscriptionOptions, stan.DurableName(c.DurableName))
	}
}

func (c *StreamingSubscriberConfig) Validate() error {
	if c.Unmarshaler == nil {
		return errors.New("StreamingSubscriberConfig.Unmarshaler is missing")
	}

	if c.QueueGroup == "" && c.SubscribersCount > 1 {
		return errors.New(
			"to set StreamingSubscriberConfig.SubscribersCount " +
				"you need to also set StreamingSubscriberConfig.QueueGroup, " +
				"in other case you will receive duplicated messages",
		)
	}

	return nil
}

type StreamingSubscriber struct {
	conn   stan.Conn
	logger watermill.LoggerAdapter

	config StreamingSubscriberConfig

	subs     []stan.Subscription
	subsLock sync.Mutex

	closed  bool
	closing chan struct{}

	outputsWg            sync.WaitGroup
	processingMessagesWg sync.WaitGroup
}

// NewStreamingSubscriber creates a new StreamingSubscriber.
//
// When using custom NATS hostname, you should pass it by options StreamingSubscriberConfig.StanOptions:
//		// ...
//		StanOptions: []stan.Option{
//			stan.NatsURL("nats://your-nats-hostname:4222"),
//		}
//		// ...
func NewStreamingSubscriber(config StreamingSubscriberConfig, logger watermill.LoggerAdapter) (*StreamingSubscriber, error) {
	config.setDefaults()

	if err := config.Validate(); err != nil {
		return nil, err
	}

	conn, err := stan.Connect(config.ClusterID, config.ClientID, config.StanOptions...)
	if err != nil {
		return nil, errors.Wrap(err, "cannot connect to NATS")
	}

	return &StreamingSubscriber{
		conn:    conn,
		logger:  logger,
		config:  config,
		closing: make(chan struct{}),
	}, nil
}

// Subscribe subscribes messages from NATS Streaming.
//
// Subscribe will spawn SubscribersCount goroutines making subscribe.
func (s *StreamingSubscriber) Subscribe(ctx context.Context, topic string) (<-chan *message.Message, error) {
	output := make(chan *message.Message, 0)
	s.outputsWg.Add(1)

	for i := 0; i < s.config.SubscribersCount; i++ {
		subscriberLogFields := watermill.LogFields{
			"subscriber_num": i,
			"topic":          topic,
		}

		s.logger.Debug("Starting subscriber", subscriberLogFields)

		sub, err := s.subscribe(ctx, output, topic, subscriberLogFields)
		if err != nil {
			return nil, errors.Wrap(err, "cannot subscribe")
		}

		go func(subscriber stan.Subscription, subscriberLogFields watermill.LogFields) {
			select {
			case <-s.closing:
				// unblock
			case <-ctx.Done():
				// unblock
			}
			if err := sub.Close(); err != nil {
				s.logger.Error("Cannot close subscriber", err, subscriberLogFields)
			}

			close(output)
			s.outputsWg.Done()
		}(sub, subscriberLogFields)

		s.subsLock.Lock()
		s.subs = append(s.subs, sub)
		s.subsLock.Unlock()
	}

	return output, nil
}

func (s *StreamingSubscriber) SubscribeInitialize(topic string) (err error) {
	sub, err := s.subscribe(context.Background(), make(chan *message.Message), topic, nil)
	if err != nil {
		return errors.Wrap(err, "cannot initialize subscribe")
	}

	return errors.Wrap(sub.Close(), "cannot close after subscribe initialize")
}

func (s *StreamingSubscriber) subscribe(
	ctx context.Context,
	output chan *message.Message,
	topic string,
	subscriberLogFields watermill.LogFields,
) (stan.Subscription, error) {
	if s.config.QueueGroup != "" {
		return s.conn.QueueSubscribe(
			topic,
			s.config.QueueGroup,
			func(m *stan.Msg) {
				s.processMessage(ctx, m, output, subscriberLogFields)
			},
			s.config.StanSubscriptionOptions...,
		)
	}

	return s.conn.Subscribe(
		topic,
		func(m *stan.Msg) {
			s.processMessage(ctx, m, output, subscriberLogFields)
		},
		s.config.StanSubscriptionOptions...,
	)
}

func (s *StreamingSubscriber) processMessage(
	ctx context.Context,
	m *stan.Msg,
	output chan *message.Message,
	logFields watermill.LogFields,
) {
	if s.closed {
		return
	}

	s.processingMessagesWg.Add(1)
	defer s.processingMessagesWg.Done()

	s.logger.Trace("Received message", logFields)

	msg, err := s.config.Unmarshaler.Unmarshal(m)
	if err != nil {
		s.logger.Error("Cannot unmarshal message", err, logFields)
		return
	}

	ctx, cancelCtx := context.WithCancel(ctx)
	msg.SetContext(ctx)
	defer cancelCtx()

	messageLogFields := logFields.Add(watermill.LogFields{"message_uuid": msg.UUID})
	s.logger.Trace("Unmarshaled message", messageLogFields)

	select {
	case output <- msg:
		s.logger.Trace("Message sent to consumer", messageLogFields)
	case <-s.closing:
		s.logger.Trace("Closing, message discarded", messageLogFields)
		return
	}

	select {
	case <-msg.Acked():
		if err := m.Ack(); err != nil {
			s.logger.Error("Cannot send ack", err, messageLogFields)
		}
		s.logger.Trace("Message Acked", messageLogFields)
	case <-msg.Nacked():
		s.logger.Trace("Message Nacked", messageLogFields)
		return
	case <-time.After(s.config.AckWaitTimeout):
		s.logger.Trace("Ack timeouted", messageLogFields)
		return
	case <-s.closing:
		s.logger.Trace("Closing, message discarded before ack", messageLogFields)
		return
	}
}

func (s *StreamingSubscriber) Close() error {
	s.subsLock.Lock()
	defer s.subsLock.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true

	s.logger.Debug("Closing subscriber", nil)
	defer s.logger.Info("StreamingSubscriber closed", nil)

	var result error

	close(s.closing)
	internalSync.WaitGroupTimeout(&s.outputsWg, s.config.CloseTimeout)

	if err := s.conn.Close(); err != nil {
		return errors.Wrap(err, "cannot close conn")
	}

	return result
}
