// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package barnacle

import (
	"fmt"
	"time"

	mproto "github.com/youtube/vitess/go/mysql/proto"
	"github.com/youtube/vitess/go/rpcplus"
	"github.com/youtube/vitess/go/vt/topo"
)

// ShardConn represents a load balanced connection to a group
// of vttablets that belong to the same shard.
type ShardConn struct {
	address    string
	keyspace   string
	shard      string
	tabletType topo.TabletType
	retryCount int
	balancer   *Balancer
	conn       *TabletConn
}

// NewShardConn creates a new ShardConn. It creates or reuses a Balancer from
// the supplied BalancerMap. retryDelay is as specified by Balancer. retryCount
// is the max number of retries before a ShardConn returns an error on an operation.
func NewShardConn(blm *BalancerMap, keyspace, shard string, tabletType topo.TabletType, retryDelay time.Duration, retryCount int) *ShardConn {
	return &ShardConn{
		keyspace:   keyspace,
		shard:      shard,
		tabletType: tabletType,
		retryCount: retryCount,
		balancer:   blm.Balancer(keyspace, shard, tabletType, retryDelay),
	}
}

func (sdc *ShardConn) connect() error {
	var lastError error
	for i := 0; i < sdc.retryCount; i++ {
		addr, err := sdc.balancer.Get()
		if err != nil {
			return err
		}
		conn, err := DialTablet(addr, sdc.keyspace, sdc.shard, "", "", false)
		if err != nil {
			lastError = err
			sdc.balancer.MarkDown(addr)
			continue
		}
		sdc.address = addr
		sdc.conn = conn
		return nil
	}
	return fmt.Errorf("could not obtain connection to %s.%s.%s, last error: %v", sdc.keyspace, sdc.shard, sdc.tabletType, lastError)
}

// Close closes the underlying vttablet connection.
func (sdc *ShardConn) Close() error {
	if sdc.conn == nil {
		return nil
	}
	err := sdc.conn.Close()
	sdc.address = ""
	sdc.conn = nil
	return err
}

func (sdc *ShardConn) mustReturn(err error) bool {
	if err == nil {
		return true
	}
	if _, ok := err.(rpcplus.ServerError); ok {
		return true
	}
	inTransaction := sdc.inTransaction()
	sdc.balancer.MarkDown(sdc.address)
	sdc.Close()
	return inTransaction
}

// ExecDirect executes a non-streaming query on vttablet. If there are connection errors,
// it retries retryCount times before failing. It does not retry if the connection is in
// the middle of a transaction.
func (sdc *ShardConn) ExecDirect(query string, bindVars map[string]interface{}) (qr *mproto.QueryResult, err error) {
	for i := 0; i < 2; i++ {
		if sdc.conn == nil {
			if err = sdc.connect(); err != nil {
				return nil, err
			}
		}
		qr, err = sdc.conn.ExecDirect(query, bindVars)
		if sdc.mustReturn(err) {
			return qr, err
		}
	}
	return qr, err
}

// ExecStream executes a streaming query on vttablet. The retry rules are the same.
func (sdc *ShardConn) ExecStream(query string, bindVars map[string]interface{}) (sr *StreamResult, err error) {
	for i := 0; i < 2; i++ {
		if sdc.conn == nil {
			if err = sdc.connect(); err != nil {
				return nil, err
			}
		}
		sr, err = sdc.conn.ExecStream(query, bindVars)
		if sdc.mustReturn(err) {
			return sr, err
		}
	}
	return sr, err
}

// Begin begins a transaction. The retry rules are the same.
func (sdc *ShardConn) Begin() (err error) {
	if sdc.inTransaction() {
		return fmt.Errorf("cannot begin: already in transaction")
	}
	for i := 0; i < 2; i++ {
		if sdc.conn == nil {
			if err = sdc.connect(); err != nil {
				return err
			}
		}
		err = sdc.conn.Begin()
		if sdc.mustReturn(err) {
			return err
		}
	}
	return err
}

// Commit commits the current transaction. There are no retries on this operation.
func (sdc *ShardConn) Commit() (err error) {
	if !sdc.inTransaction() {
		return fmt.Errorf("cannot commit: not in transaction")
	}
	return sdc.conn.Commit()
}

// Rollback rolls back the current transaction. There are no retries on this operation.
func (sdc *ShardConn) Rollback() (err error) {
	if !sdc.inTransaction() {
		return fmt.Errorf("cannot rollback: not in transaction")
	}
	return sdc.conn.Rollback()
}

func (sdc *ShardConn) inTransaction() bool {
	return sdc.conn != nil && sdc.conn.TransactionId != 0
}
