//
// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.
//

package pulsar

import (
    `context`
    `fmt`
    `github.com/apache/pulsar-client-go/pkg/pb`
    `github.com/apache/pulsar-client-go/pulsar/internal`
    `github.com/apache/pulsar-client-go/util`
    `github.com/golang/protobuf/proto`
    log "github.com/sirupsen/logrus"
    `math`
    `sync`
    `time`
)

const maxRedeliverUnacknowledged = 1000

type consumerState int

const (
    consumerInit = iota
    consumerReady
    consumerClosing
    consumerClosed
)

var (
    subType  pb.CommandSubscribe_SubType
    position pb.CommandSubscribe_InitialPosition
)

type partitionConsumer struct {
    state  consumerState
    client *client
    topic  string
    log    *log.Entry
    cnx    internal.Connection

    options      *ConsumerOptions
    consumerName *string
    consumerID   uint64

    omu      sync.Mutex // protects following
    overflow []*pb.MessageIdData

    unAckTracker *UnackedMessageTracker

    eventsChan   chan interface{}
    partitionIdx int
}

func newPartitionConsumer(client *client, topic string, options *ConsumerOptions, partitionId int) (*partitionConsumer, error) {
    c := &partitionConsumer{
        state:        consumerInit,
        client:       client,
        topic:        topic,
        options:      options,
        log:          log.WithField("topic", topic),
        consumerID:   client.rpcClient.NewConsumerId(),
        partitionIdx: partitionId,
        eventsChan:   make(chan interface{}, 10),
    }

    c.setDefault(options)

    if options.MessageChannel == nil {
        options.MessageChannel = make(chan ConsumerMessage, options.ReceiverQueueSize)
    }

    if options.Name != "" {
        c.consumerName = &options.Name
    }

    switch options.Type {
    case Exclusive:
        subType = pb.CommandSubscribe_Exclusive
    case Failover:
        subType = pb.CommandSubscribe_Failover
    case Shared:
        subType = pb.CommandSubscribe_Shared
    case KeyShared:
        subType = pb.CommandSubscribe_Key_Shared
    }

    if options.Type == Shared || options.Type == KeyShared {
        if options.AckTimeout != 0 {
            c.unAckTracker = NewUnackedMessageTracker()
            c.unAckTracker.pc = c
            c.unAckTracker.Start(int64(options.AckTimeout))
        }
    }

    switch options.SubscriptionInitPos {
    case Latest:
        position = pb.CommandSubscribe_Latest
    case Earliest:
        position = pb.CommandSubscribe_Earliest
    }

    err := c.grabCnx()
    if err != nil {
        log.WithError(err).Errorf("Failed to create consumer")
        return nil, err
    } else {
        c.log = c.log.WithField("name", c.consumerName)
        c.log.Info("Created consumer")
        c.state = consumerReady
        go c.runEventsLoop()

        return c, nil
    }
}

func (pc *partitionConsumer) setDefault(options *ConsumerOptions) {
    if options.ReceiverQueueSize <= 0 {
        options.ReceiverQueueSize = 1000
    }

    if options.AckTimeout == 0 {
        options.AckTimeout = time.Second * 30
    }

    position = pb.CommandSubscribe_Latest
    subType = pb.CommandSubscribe_Exclusive
}

func (pc *partitionConsumer) grabCnx() error {
    lr, err := pc.client.lookupService.Lookup(pc.topic)
    if err != nil {
        pc.log.WithError(err).Warn("Failed to lookup topic")
        return err
    }

    pc.log.Debug("Lookup result: ", lr)
    requestID := pc.client.rpcClient.NewRequestId()
    res, err := pc.client.rpcClient.Request(lr.LogicalAddr, lr.PhysicalAddr, requestID,
        pb.BaseCommand_SUBSCRIBE, &pb.CommandSubscribe{
            RequestId:       proto.Uint64(requestID),
            Topic:           &pc.topic,
            SubType:         subType.Enum(),
            Subscription:    proto.String(pc.options.SubscriptionName),
            ConsumerId:      proto.Uint64(pc.consumerID),
            ConsumerName:    proto.String(pc.options.Name),
            InitialPosition: position.Enum(),
            Schema:          nil,
        })

    if err != nil {
        pc.log.WithError(err).Error("Failed to create consumer")
        return err
    }

    if res.Response.ConsumerStatsResponse != nil {
        pc.consumerName = res.Response.ConsumerStatsResponse.ConsumerName
    }

    pc.cnx = res.Cnx
    pc.log.WithField("cnx", res.Cnx).Debug("Connected consumer")

    msgType := res.Response.GetType()

    switch msgType {
    case pb.BaseCommand_SUCCESS:
        pc.cnx.AddConsumeHandler(pc.consumerID, pc)
        if err := pc.internalFlow(uint32(pc.options.ReceiverQueueSize)); err != nil {
            return err
        }
        return nil
    case pb.BaseCommand_ERROR:
        errMsg := res.Response.GetError()
        return fmt.Errorf("%s: %s", errMsg.GetError().String(), errMsg.GetMessage())
    default:
        return util.NewUnexpectedErrMsg(msgType, requestID)
    }
}

func (pc *partitionConsumer) Topic() string {
    return pc.topic
}

func (pc *partitionConsumer) Subscription() string {
    return pc.options.SubscriptionName
}

func (pc *partitionConsumer) Unsubscribe() error {
    wg := &sync.WaitGroup{}
    wg.Add(1)

    hu := &handleUnsubscribe{
        waitGroup: wg,
        err:       nil,
    }
    pc.eventsChan <- hu

    wg.Wait()
    return hu.err
}

func (pc *partitionConsumer) internalUnsubscribe(unsub *handleUnsubscribe) {
    requestID := pc.client.rpcClient.NewRequestId()
    _, err := pc.client.rpcClient.RequestOnCnx(pc.cnx, requestID,
        pb.BaseCommand_UNSUBSCRIBE, &pb.CommandUnsubscribe{
            RequestId:  proto.Uint64(requestID),
            ConsumerId: proto.Uint64(pc.consumerID),
        })
    if err != nil {
        pc.log.WithError(err).Error("Failed to unsubscribe consumer")
        unsub.err = err
    }

    pc.cnx.DeleteConsumeHandler(pc.consumerID)
    if pc.unAckTracker != nil {
        pc.unAckTracker.Stop()
    }

    unsub.waitGroup.Done()
}

func (pc *partitionConsumer) Receive(ctx context.Context) (Message, error) {
    select {
    case <-ctx.Done():
        return nil, ctx.Err()
    case cm, ok := <-pc.options.MessageChannel:
        if ok {
            id := &pb.MessageIdData{}
            err := proto.Unmarshal(cm.ID().Serialize(), id)
            if err != nil {
                pc.log.WithError(err).Errorf("unserialize message id error:%s", err.Error())
                return nil, err
            }
            if pc.unAckTracker != nil {
                pc.unAckTracker.Add(id)
            }
            return cm.Message, nil
        }
        return nil, newError(ResultConnectError, "receive queue closed")
    }
}

func (pc *partitionConsumer) ReceiveAsync(ctx context.Context, msgs chan<- ConsumerMessage) error {
    highwater := uint32(math.Max(float64(cap(pc.options.MessageChannel)/2), 1))

    // request half the buffer's capacity
    if err := pc.internalFlow(highwater); err != nil {
        pc.log.Errorf("Send Flow cmd error:%s", err.Error())
        return err
    }
    var receivedSinceFlow uint32

    for {
        select {
        case tmpMsg, ok := <-pc.options.MessageChannel:
            if ok {
                msgs <- tmpMsg
                id := &pb.MessageIdData{}
                err := proto.Unmarshal(tmpMsg.ID().Serialize(), id)
                if err != nil {
                    pc.log.WithError(err).Errorf("unserialize message id error:%s", err.Error())
                    return err
                }
                if pc.unAckTracker != nil {
                    pc.unAckTracker.Add(id)
                }
                receivedSinceFlow++
                if  receivedSinceFlow >= highwater {
                    if err := pc.internalFlow(receivedSinceFlow); err != nil {
                        pc.log.Errorf("Send Flow cmd error:%s", err.Error())
                        return err
                    }
                    receivedSinceFlow = 0
                }
                continue
            }
        case <-ctx.Done():
            return ctx.Err()
        }
    }

}

func (pc *partitionConsumer) Ack(msg Message) error {
    return pc.AckID(msg.ID())
}

func (pc *partitionConsumer) AckID(msgID MessageID) error {
    wg := &sync.WaitGroup{}
    wg.Add(1)
    ha := &handleAck{
        msgID:     msgID,
        waitGroup: wg,
        err:       nil,
    }
    pc.eventsChan <- ha
    wg.Wait()
    return ha.err
}

func (pc *partitionConsumer) internalAck(ack *handleAck) {
    id := &pb.MessageIdData{}
    messageIDs := make([]*pb.MessageIdData, 0)
    err := proto.Unmarshal(ack.msgID.Serialize(), id)
    if err != nil {
        pc.log.WithError(err).Errorf("unserialize message id error:%s", err.Error())
        ack.err = err
    }

    messageIDs = append(messageIDs, id)

    requestID := pc.client.rpcClient.NewRequestId()
    _, err = pc.client.rpcClient.RequestOnCnxNoWait(pc.cnx, requestID,
        pb.BaseCommand_ACK, &pb.CommandAck{
            ConsumerId: proto.Uint64(pc.consumerID),
            MessageId:  messageIDs,
            AckType:    pb.CommandAck_Individual.Enum(),
        })
    if err != nil {
        pc.log.WithError(err).Error("Failed to unsubscribe consumer")
        ack.err = err
    }

    if pc.unAckTracker != nil {
        pc.unAckTracker.Remove(id)
    }
    ack.waitGroup.Done()
}

func (pc *partitionConsumer) AckCumulative(msg Message) error {
    return pc.AckCumulativeID(msg.ID())
}

func (pc *partitionConsumer) AckCumulativeID(msgID MessageID) error {
    hac := &handleAckCumulative{
        msgID: msgID,
        err:   nil,
    }
    pc.eventsChan <- hac

    return hac.err
}

func (pc *partitionConsumer) internalAckCumulative(ackCumulative *handleAckCumulative) {
    id := &pb.MessageIdData{}
    messageIDs := make([]*pb.MessageIdData, 0)
    err := proto.Unmarshal(ackCumulative.msgID.Serialize(), id)
    if err != nil {
        pc.log.WithError(err).Errorf("unserialize message id error:%s", err.Error())
        ackCumulative.err = err
    }
    messageIDs = append(messageIDs, id)

    requestID := pc.client.rpcClient.NewRequestId()
    _, err = pc.client.rpcClient.RequestOnCnx(pc.cnx, requestID,
        pb.BaseCommand_ACK, &pb.CommandAck{
            ConsumerId: proto.Uint64(pc.consumerID),
            MessageId:  messageIDs,
            AckType:    pb.CommandAck_Cumulative.Enum(),
        })
    if err != nil {
        pc.log.WithError(err).Error("Failed to unsubscribe consumer")
        ackCumulative.err = err
    }

    if pc.unAckTracker != nil {
        pc.unAckTracker.Remove(id)
    }
}

func (pc *partitionConsumer) Close() error {
    if pc.state != consumerReady {
        return nil
    }
    if pc.unAckTracker != nil {
        pc.unAckTracker.Stop()
    }

    wg := sync.WaitGroup{}
    wg.Add(1)

    cc := &handlerClose{&wg, nil}
    pc.eventsChan <- cc

    wg.Wait()
    return cc.err
}

func (pc *partitionConsumer) Seek(msgID MessageID) error {
    hc := &handleSeek{
        msgID: msgID,
        err:   nil,
    }
    pc.eventsChan <- hc
    return hc.err
}

func (pc *partitionConsumer) internalSeek(seek *handleSeek) {
    id := &pb.MessageIdData{}
    err := proto.Unmarshal(seek.msgID.Serialize(), id)
    if err != nil {
        pc.log.WithError(err).Errorf("unserialize message id error:%s", err.Error())
        seek.err = err
    }

    requestID := pc.client.rpcClient.NewRequestId()
    _, err = pc.client.rpcClient.RequestOnCnx(pc.cnx, requestID,
        pb.BaseCommand_SEEK, &pb.CommandSeek{
            ConsumerId: proto.Uint64(pc.consumerID),
            RequestId:  proto.Uint64(requestID),
            MessageId:  id,
        })
    if err != nil {
        pc.log.WithError(err).Error("Failed to unsubscribe consumer")
        seek.err = err
    }
}

func (pc *partitionConsumer) RedeliverUnackedMessages() error {
    wg := &sync.WaitGroup{}
    wg.Add(1)

    hr := &handleRedeliver{
        waitGroup: wg,
        err:       nil,
    }
    pc.eventsChan <- hr
    wg.Wait()
    return hr.err
}

func (pc *partitionConsumer) internalRedeliver(redeliver *handleRedeliver) {
    pc.omu.Lock()
    defer pc.omu.Unlock()

    overFlowSize := len(pc.overflow)

    if overFlowSize == 0 {
        return
    }

    requestID := pc.client.rpcClient.NewRequestId()

    for i := 0; i < len(pc.overflow); i += maxRedeliverUnacknowledged {
        end := i + maxRedeliverUnacknowledged
        if end > overFlowSize {
            end = overFlowSize
        }
        _, err := pc.client.rpcClient.RequestOnCnx(pc.cnx, requestID,
            pb.BaseCommand_REDELIVER_UNACKNOWLEDGED_MESSAGES, &pb.CommandRedeliverUnacknowledgedMessages{
                ConsumerId: proto.Uint64(pc.consumerID),
                MessageIds: pc.overflow[i:end],
            })
        if err != nil {
            pc.log.WithError(err).Error("Failed to unsubscribe consumer")
            redeliver.err = err
        }
    }

    // clear Overflow slice
    pc.overflow = nil

    if pc.unAckTracker != nil {
        pc.unAckTracker.clear()
    }
    redeliver.waitGroup.Done()
}

func (pc *partitionConsumer) runEventsLoop() {
    for {
        select {
        case i := <-pc.eventsChan:
            switch v := i.(type) {
            case *handlerClose:
                pc.internalClose(v)
                return
            case *handleSeek:
                pc.internalSeek(v)
            case *handleUnsubscribe:
                pc.internalUnsubscribe(v)
            case *handleAckCumulative:
                pc.internalAckCumulative(v)
            case *handleAck:
                pc.internalAck(v)
            case *handleRedeliver:
                pc.internalRedeliver(v)
            }
        }
    }
}

func (pc *partitionConsumer) internalClose(req *handlerClose) {
    if pc.state != consumerReady {
        req.waitGroup.Done()
        return
    }

    pc.state = consumerClosing
    pc.log.Info("Closing consumer")

    requestID := pc.client.rpcClient.NewRequestId()
    _, err := pc.client.rpcClient.RequestOnCnx(pc.cnx, requestID, pb.BaseCommand_CLOSE_CONSUMER, &pb.CommandCloseConsumer{
        ConsumerId: &pc.consumerID,
        RequestId:  &requestID,
    })
    pc.cnx.DeleteConsumeHandler(pc.consumerID)

    if err != nil {
        req.err = err
    } else {
        pc.log.Info("Closed consumer")
        pc.state = consumerClosed
        //pc.cnx.UnregisterListener(pc.consumerID)
    }

    req.waitGroup.Done()
}

// Flow command gives additional permits to send messages to the consumer.
// A typical consumer implementation will use a queue to accuMulate these messages
// before the application is ready to consume them. After the consumer is ready,
// the client needs to give permission to the broker to push messages.
func (pc *partitionConsumer) internalFlow(permits uint32) error {
    if permits <= 0 {
        return fmt.Errorf("invalid number of permits requested: %d", permits)
    }

    requestID := pc.client.rpcClient.NewRequestId()
    _, err := pc.client.rpcClient.RequestOnCnxNoWait(pc.cnx, requestID,
        pb.BaseCommand_FLOW, &pb.CommandFlow{
            ConsumerId:     proto.Uint64(pc.consumerID),
            MessagePermits: proto.Uint32(permits),
        })

    if err != nil {
        pc.log.WithError(err).Error("Failed to unsubscribe consumer")
        return err
    }
    return nil
}

func (pc *partitionConsumer) HandlerMessage(response *pb.CommandMessage, headersAndPayload []byte) error {
    msgID := response.GetMessageId()

    id := newMessageId(int64(msgID.GetLedgerId()), int64(msgID.GetEntryId()),
        pc.partitionIdx, int(msgID.GetBatchIndex()))

    msgMeta, payload, err := internal.ParseMessage(headersAndPayload)
    if err != nil {
        return fmt.Errorf("parse message error:%s", err)
    }

    //numMsgs := msgMeta.GetNumMessagesInBatch()

    msg := &message{
        publishTime: timeFromUnixTimestampMillis(msgMeta.GetPublishTime()),
        eventTime:   timeFromUnixTimestampMillis(msgMeta.GetEventTime()),
        key:         msgMeta.GetPartitionKey(),
        properties:  internal.ConvertToStringMap(msgMeta.GetProperties()),
        topic:       pc.topic,
        msgID:       id,
        payLoad:     payload,
    }

    consumerMsg := ConsumerMessage{
        Message:  msg,
        Consumer: pc,
    }

    select {
    case pc.options.MessageChannel <- consumerMsg:
        return nil
    default:
        // Add messageId to Overflow buffer, avoiding duplicates.
        newMid := response.GetMessageId()
        var dup bool

        pc.omu.Lock()
        for _, mid := range pc.overflow {
            if proto.Equal(mid, newMid) {
                dup = true
                break
            }
        }

        if !dup {
            pc.overflow = append(pc.overflow, newMid)
        }
        pc.omu.Unlock()
        return fmt.Errorf("consumer message queue on topic %s is full (capacity = %d)", pc.Topic(), cap(pc.options.MessageChannel))
    }
}

type handleAck struct {
    msgID     MessageID
    waitGroup *sync.WaitGroup
    err       error
}

type handleAckCumulative struct {
    msgID MessageID
    err   error
}

type handleUnsubscribe struct {
    waitGroup *sync.WaitGroup
    err       error
}

type handleSeek struct {
    msgID MessageID
    err   error
}

type handleRedeliver struct {
    waitGroup *sync.WaitGroup
    err       error
}

type handlerClose struct {
    waitGroup *sync.WaitGroup
    err       error
}

type handleConnectionClosed struct{}

func (pc *partitionConsumer) ConnectionClosed() {
    // Trigger reconnection in the produce goroutine
    pc.eventsChan <- &connectionClosed{}
}

func (pc *partitionConsumer) reconnectToBroker() {
    pc.log.Info("Reconnecting to broker")
    backoff := internal.Backoff{}
    for {
        if pc.state != consumerReady {
            // Consumer is already closing
            return
        }

        err := pc.grabCnx()
        if err == nil {
            // Successfully reconnected
            return
        }

        d := backoff.Next()
        pc.log.Info("Retrying reconnection after ", d)

        time.Sleep(d)
    }
}
