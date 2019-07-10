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
    "github.com/apache/pulsar-client-go/pkg/pb"
    "github.com/golang/protobuf/proto"
    `time`
)

type messageId struct {
    ledgerID     int64
    entryID      int64
    batchIdx     int
    partitionIdx int
}

func newMessageId(ledgerID int64, entryID int64, batchIdx int, partitionIdx int) MessageID {
    return &messageId{
        ledgerID:     ledgerID,
        entryID:      entryID,
        batchIdx:     batchIdx,
        partitionIdx: partitionIdx,
    }
}

func (id *messageId) Serialize() []byte {
    msgId := &pb.MessageIdData{
        LedgerId:   proto.Uint64(uint64(id.ledgerID)),
        EntryId:    proto.Uint64(uint64(id.entryID)),
        BatchIndex: proto.Int(id.batchIdx),
        Partition:  proto.Int(id.partitionIdx),
    }
    data, _ := proto.Marshal(msgId)
    return data
}

func deserializeMessageId(data []byte) (MessageID, error) {
    msgId := &pb.MessageIdData{}
    err := proto.Unmarshal(data, msgId)
    if err != nil {
        return nil, err
    } else {
        id := newMessageId(
            int64(msgId.GetLedgerId()),
            int64(msgId.GetEntryId()),
            int(msgId.GetBatchIndex()),
            int(msgId.GetPartition()),
        )
        return id, nil
    }
}

func earliestMessageID() MessageID {
    return newMessageId(-1, -1, -1, -1)
}

const maxLong int64 = 0x7fffffffffffffff

func latestMessageID() MessageID {
    return newMessageId(maxLong, maxLong, -1, -1)
}

func timeFromUnixTimestampMillis(timestamp uint64) time.Time {
    ts := int64(timestamp) * int64(time.Millisecond)
    seconds := ts / int64(time.Second)
    nanos := ts - (seconds * int64(time.Second))
    return time.Unix(seconds, nanos)
}

func timeToUnixTimestampMillis(t time.Time) uint64 {
    nanos := t.UnixNano()
    millis := nanos / int64(time.Millisecond)
    return uint64(millis)
}

type message struct {
    publishTime time.Time
    eventTime   time.Time
    key         string
    payLoad     []byte
    msgID       MessageID
    properties  map[string]string
    topic       string
}

func (msg *message) Topic() string {
    return msg.topic
}

func (msg *message) Properties() map[string]string {
    return msg.properties
}

func (msg *message) Payload() []byte {
    return msg.payLoad
}

func (msg *message) ID() MessageID {
    return msg.msgID
}

func (msg *message) PublishTime() time.Time {
    return msg.publishTime
}

func (msg *message) EventTime() time.Time {
    return msg.eventTime
}

func (msg *message) Key() string {
    return msg.key
}
