// Copyright 2013, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wrangler

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/youtube/vitess/go/vt/key"
	"github.com/youtube/vitess/go/vt/mysqlctl"
	tm "github.com/youtube/vitess/go/vt/tabletmanager"
	"github.com/youtube/vitess/go/vt/topo"
	"github.com/youtube/vitess/go/vt/zktopo"
)

// createTestTablet creates the test tablet in the topology.
// 'uid' has to be between 0 and 99. All the ports will be derived from that.
func createTestTablet(t *testing.T, wr *Wrangler, cell string, uid uint32, tabletType topo.TabletType, parent topo.TabletAlias) topo.TabletAlias {
	if uid < 0 || uid > 99 {
		t.Fatalf("uid has to be between 0 and 99: %v", uid)
	}
	state := topo.STATE_READ_ONLY
	if tabletType == topo.TYPE_MASTER {
		state = topo.STATE_READ_WRITE
	}
	if err := wr.InitTablet(&topo.Tablet{
		Cell:           cell,
		Uid:            100 + uid,
		Parent:         parent,
		Addr:           fmt.Sprintf("%vhost:%v", cell, 8100+uid),
		SecureAddr:     fmt.Sprintf("%vhost:%v", cell, 8200+uid),
		MysqlAddr:      fmt.Sprintf("%vhost:%v", cell, 3300+uid),
		MysqlIpAddr:    fmt.Sprintf("%v.0.0.1:%v", 100+uid, 3300+uid),
		Keyspace:       "test_keyspace",
		Shard:          "0",
		Type:           tabletType,
		State:          state,
		DbNameOverride: "",
		KeyRange:       key.KeyRange{},
	}, false, true, false); err != nil {
		t.Fatalf("cannot create tablet %v: %v", uid, err)
	}
	return topo.TabletAlias{cell, 100 + uid}
}

// startFakeTabletActionLoop will start the action loop for a fake tablet,
// using mysqlDaemon as the backing mysqld.
func startFakeTabletActionLoop(t *testing.T, wr *Wrangler, tabletAlias topo.TabletAlias, mysqlDaemon mysqlctl.MysqlDaemon, done chan struct{}) {
	go func() {
		f := func(actionPath, data string) error {
			actionNode, err := tm.ActionNodeFromJson(data, actionPath)
			if err != nil {
				t.Fatalf("ActionNodeFromJson failed: %v\n%v", err, data)
			}
			ta := tm.NewTabletActor(nil, mysqlDaemon, wr.ts, tabletAlias)
			if err := ta.HandleAction(actionPath, actionNode.Action, actionNode.ActionGuid, false); err != nil {
				// action may just fail for any good reason
				t.Logf("HandleAction failed for %v: %v", actionNode.Action, err)
			}
			return nil
		}
		wr.ts.ActionEventLoop(tabletAlias, f, done)
	}()
}

func TestShardExternallyReparented(t *testing.T) {
	ts := zktopo.NewTestServer(t, []string{"cell1", "cell2"})
	wr := New(ts, time.Minute, time.Second)

	// Create a master and a replica
	oldMasterAlias := createTestTablet(t, wr, "cell1", 0, topo.TYPE_MASTER, topo.TabletAlias{})
	newMasterAlias := createTestTablet(t, wr, "cell1", 1, topo.TYPE_REPLICA, oldMasterAlias)
	goodSlaveAlias1 := createTestTablet(t, wr, "cell1", 2, topo.TYPE_REPLICA, oldMasterAlias)
	goodSlaveAlias2 := createTestTablet(t, wr, "cell2", 3, topo.TYPE_REPLICA, oldMasterAlias)
	badSlaveAlias := createTestTablet(t, wr, "cell1", 4, topo.TYPE_REPLICA, oldMasterAlias)

	// First test: reparent to the same master, make sure it works
	// as expected.
	if err := wr.ShardExternallyReparented("test_keyspace", "0", oldMasterAlias, false, 80); err == nil {
		t.Fatalf("ShardExternallyReparented(same master) should have failed")
	} else {
		if !strings.Contains(err.Error(), "already master") {
			t.Fatalf("ShardExternallyReparented(same master) should have failed with an error that contains 'already master' but got: %v", err)
		}
	}

	// Second test: reparent to the replica, and pretend the old
	// master is still good to go.
	done := make(chan struct{}, 1)

	// On the elected master, we will only respond to
	// TABLET_ACTION_SLAVE_WAS_PROMOTED, no need for a MysqlDaemon
	startFakeTabletActionLoop(t, wr, newMasterAlias, nil, done)

	// On the old master, we will only respond to
	// TABLET_ACTION_SLAVE_WAS_RESTARTED.
	oldMasterMysqlDaemon := &mysqlctl.FakeMysqlDaemon{
		MasterAddr: "101.0.0.1:3301",
	}
	startFakeTabletActionLoop(t, wr, oldMasterAlias, oldMasterMysqlDaemon, done)

	// On the good slaves, we will respond to
	// TABLET_ACTION_SLAVE_WAS_RESTARTED.
	goodSlaveMysqlDaemon1 := &mysqlctl.FakeMysqlDaemon{
		MasterAddr: "101.0.0.1:3301",
	}
	startFakeTabletActionLoop(t, wr, goodSlaveAlias1, goodSlaveMysqlDaemon1, done)
	goodSlaveMysqlDaemon2 := &mysqlctl.FakeMysqlDaemon{
		MasterAddr: "101.0.0.1:3301",
	}
	startFakeTabletActionLoop(t, wr, goodSlaveAlias2, goodSlaveMysqlDaemon2, done)

	// On the bad slave, we will respond to
	// TABLET_ACTION_SLAVE_WAS_RESTARTED.
	badSlaveMysqlDaemon := &mysqlctl.FakeMysqlDaemon{
		MasterAddr: "234.0.0.1:3301",
	}
	startFakeTabletActionLoop(t, wr, badSlaveAlias, badSlaveMysqlDaemon, done)

	if err := wr.ShardExternallyReparented("test_keyspace", "0", newMasterAlias, false, 60); err != nil {
		t.Fatalf("ShardExternallyReparented(replica) failed: %v", err)
	}
	close(done)
}
