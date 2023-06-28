package pubsub

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ntons/libra-go"
	v1pb "github.com/ntons/libra-go/api/libra/v1"
	"github.com/ntons/log-go"
	"github.com/ntons/redis"
	"google.golang.org/protobuf/proto"
)

type pubSubServer struct {
	v1pb.UnimplementedPubSubServiceServer
}

func newPubSubServer() *pubSubServer { return &pubSubServer{} }

func getAppId(ctx context.Context) (appId string, err error) {
	if trusted := libra.RequireAuthBySecret(ctx); trusted == nil {
		return "", errUnauthenticated
	} else {
		return trusted.AppId, nil
	}
}

func toStream(appId, topic string) string {
	if strings.ContainsRune(appId, ':') {
		panic("invalid app id")
	}
	return fmt.Sprintf("%s:%s", appId, topic)
}

func toTopic(stream string) string {
	i := strings.IndexRune(stream, ':')
	if i < 0 {
		panic("invalid stream")
	}
	return stream[i+1:]
}

func parseXMessage(m redis.XMessage, topic string) (*v1pb.PubSub_Message, error) {
	v, ok := m.Values["pubsub"]
	if !ok {
		return nil, fmt.Errorf("failed to get pubsub message value")
	}
	b, err := base64.StdEncoding.DecodeString(v.(string))
	if err != nil {
		return nil, fmt.Errorf("failed to decode pubsub message: %e", err)
	}
	r := &v1pb.PubSub_Message{}
	if err = proto.Unmarshal(b, r); err != nil {
		return nil, fmt.Errorf("failed to unmarshal pubsub message: %v", err)
	}
	r.Topic = topic
	r.Id = m.ID
	return r, nil
}

func (*pubSubServer) Publish(
	ctx context.Context, req *v1pb.PubSub_PublishRequest) (
	_ *v1pb.PubSub_PublishResponse, err error) {

	appId, err := getAppId(ctx)
	if err != nil {
		return
	}

	args := &redis.XAddArgs{
		Approx: true,
		ID:     "*",
	}
	if req.Opts != nil {
		args.NoMkStream = !req.Opts.CreateTopic
		args.MaxLen = req.Opts.MaxLen
		args.MinID = req.Opts.MinId
	}

	for _, msg := range req.Msgs {
		var b []byte
		if b, err = proto.Marshal(msg); err != nil {
			err = newInvalidArgumentError("bad msg")
			return
		}
		args.Stream = toStream(appId, msg.Topic)
		args.Values = append([]any{}, "pubsub", base64.StdEncoding.EncodeToString(b))
		if err = cli.XAdd(ctx, args).Err(); err != nil {
			return
		}
	}

	return &v1pb.PubSub_PublishResponse{}, nil
}

func (*pubSubServer) Subscribe(
	req *v1pb.PubSub_SubscribeRequest,
	sess v1pb.PubSubService_SubscribeServer) error {

	appId, err := getAppId(sess.Context())
	if err != nil {
		return err
	}

	var (
		wg sync.WaitGroup // reading goroutine waitgroup
		mu sync.Mutex     // sending mutex
	)
	defer wg.Wait()

	// 多个topic可能存在不同散列，需要分开read
	for _, sub := range req.Subscriptions {
		id := fmt.Sprintf("%d-0", sub.SinceMilliTimestamp)
		if sub.AfterId != "" {
			a := strings.SplitN(sub.AfterId, "-", 2)
			if v, err := strconv.ParseInt(a[0], 10, 64); err != nil {
				return newInvalidArgumentError(
					"invalid subscription after id: %v", sub.AfterId)
			} else if v >= sub.SinceMilliTimestamp {
				id = sub.AfterId
			}
		}
		wg.Add(1)
		go func(args *redis.XReadArgs) {
			defer wg.Done()
			for {
				r, err := cli.XRead(sess.Context(), args).Result()
				if err != nil {
					log.Warnf("failed to read pubsub message: %v", err)
					return
				}
				var resp = &v1pb.PubSub_SubscribeResponse{}
				for _, e := range r {
					topic := toTopic(e.Stream)
					for _, m := range e.Messages {
						args.Streams[1] = m.ID
						msg, err := parseXMessage(m, topic)
						if err != nil {
							log.Warnf("%v", err)
							continue
						}
						resp.Msgs = append(resp.Msgs, msg)
					}
				}
				if len(resp.Msgs) == 0 {
					continue
				}
				mu.Lock()
				err = sess.Send(resp)
				mu.Unlock()
				if err != nil {
					log.Warnf("failed to send pubsub response: %v", err)
					return
				}
			}
		}(&redis.XReadArgs{
			Streams: []string{toStream(appId, sub.Topic), id},
			Count:   int64(sub.BatchSize),
		})
	}

	return nil
}

func (srv *pubSubServer) Consume(
	ctx context.Context, req *v1pb.PubSub_ConsumeRequest) (
	_ *v1pb.PubSub_ConsumeResponse, err error) {
	appId, err := getAppId(ctx)
	if err != nil {
		return
	}

	if err = srv.ack(ctx, appId, req.Acks); err != nil {
		return
	}

	var (
		wg   sync.WaitGroup // reading goroutine waitgroup
		mu   sync.Mutex     // sending mutex
		resp = &v1pb.PubSub_ConsumeResponse{}
	)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, con := range req.Consumptions {
		wg.Add(1)
		go func(con *v1pb.PubSub_Consumption) {
			defer wg.Done()
			defer cancel()

			msgs, err := srv.readGroup(ctx, appId, con)
			if err != nil {
				return
			}

			mu.Lock()
			defer mu.Unlock()
			resp.Msgs = append(resp.Msgs, msgs...)
		}(con)
	}

	wg.Wait() // waiting all reading goroutine join

	return resp, nil
}

func (*pubSubServer) ack(
	ctx context.Context, appId string, acks []*v1pb.PubSub_Ack) error {
	for _, ack := range acks {
		var (
			stream = toStream(appId, ack.Topic)
			group  = fmt.Sprintf("%d", ack.GroupId)
		)
		if err := cli.XAck(ctx, stream, group, ack.MsgIds...).Err(); err != nil {
			log.Warnf("failed to ack: %v", err)
			return newUnavailableError("db error")
		}
	}
	return nil
}

func (*pubSubServer) createGroup(
	ctx context.Context, stream, group string) (err error) {
	if err = cli.XGroupCreateMkStream(
		ctx, stream, group, "0-0").Err(); isBusyGroupError(err) {
		err = nil // 有可能同时被其他消费者创建
	}
	return
}

func (srv *pubSubServer) readGroup(
	ctx context.Context, appId string, con *v1pb.PubSub_Consumption) (
	msgs []*v1pb.PubSub_Message, err error) {
	var (
		stream  = toStream(appId, con.Topic)
		group   = fmt.Sprintf("%d", con.GroupId)
		timeout = time.Duration(con.AckTimeoutMilli) * time.Millisecond

		xAutoClaimArgs = &redis.XAutoClaimArgs{
			Stream:   stream,
			Group:    group,
			MinIdle:  timeout,
			Start:    "0-0",
			Count:    1,
			Consumer: group,
		}

		xReadGroupArgs = &redis.XReadGroupArgs{
			Group:    group,
			Consumer: group,
			Streams:  []string{stream, ">"},
			Count:    1,
			Block:    timeout, // 轮询autoclaim
		}
	)

	for len(msgs) == 0 {
		var r []redis.XMessage

	xAutoClaim:
		if r, _, err = cli.XAutoClaim(ctx, xAutoClaimArgs).Result(); err != nil {
			if isNoGroupError(err) {
				if err = srv.createGroup(ctx, stream, group); err == nil {
					goto xAutoClaim
				}
			}
			if !isCanceledError(err) {
				log.Warnf("failed to claim: %v", err)
			}
			return
		}

	xReadGroup:
		if len(r) == 0 {
			var r1 []redis.XStream
			r1, err = cli.XReadGroup(ctx, xReadGroupArgs).Result()
			if err != nil && err != redis.Nil {
				if isNoGroupError(err) {
					if err = srv.createGroup(ctx, stream, group); err == nil {
						goto xReadGroup
					}
				}
				if !isCanceledError(err) {
					log.Warnf("failed to read group: %v", err)
				}
				return
			}
			for _, e := range r1 {
				if e.Stream != stream {
					log.Errorf("read group got mismatched stream: %v, %v", e.Stream, stream)
					continue
				}
				r = append(r, e.Messages...)
			}
		}

		for _, m := range r {
			if msg, err := parseXMessage(m, con.Topic); err != nil {
				log.Warnf("failed to parse message: %v", err)
			} else {
				msgs = append(msgs, msg)
			}
		}
	}
	return
}
