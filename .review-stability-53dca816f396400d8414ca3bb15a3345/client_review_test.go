package client

import (
 "context"
 "net"
 "testing"
 "time"
)

func TestReviewMigratedSnapshotMustReachDisk(t *testing.T) {
 root := t.TempDir()
 path := StatePath(root)
 before := Snapshot{DefaultGateway:"192.168.1.1", DefaultIfIndex:7, Interfaces:[]IfaceDNS{{Name:"old", DNS:[]string{"192.168.1.1"}}}}
 after := Snapshot{DefaultGateway:"192.168.2.1", DefaultIfIndex:9, Interfaces:[]IfaceDNS{{Name:"new", DNS:[]string{"192.168.2.1"}}}}
 if err := SaveSnapshot(path,before); err != nil {t.Fatal(err)}
 a := &App{rootDir:root, statePath:path, snapshot:before}
 a.updateSnapshot(after)
 disk, _, err := LoadSnapshot(path)
 if err != nil {t.Fatal(err)}
 if disk.DefaultIfIndex != after.DefaultIfIndex {t.Fatalf("migration updated RAM to interface %d but recovery file still says %d",after.DefaultIfIndex,disk.DefaultIfIndex)}
}

func TestReviewHostnameNeedsBypassRoute(t *testing.T) {
 cfg:=NetConfig{ServerIP:"tunnel.example.com", Gateway:"10.10.0.1"}
 snap:=newSnapshot("192.168.1.1",7,"192.168.1.1",nil,cfg)
 ops:=routePlan(snap,cfg)
 for _,op:=range ops {if !op.onTunnel && op.dest.Bits()==32 {return}}
 t.Fatalf("hostname config has %d tunnel routes and no server bypass; resolved IPv4 was never stored",len(ops))
}

func TestReviewBusyDisconnectMustNotClaimCompleted(t *testing.T) {
 a:=&App{disconnecting:true}
 if err:=a.Disconnect();err==nil {t.Fatal("Disconnect returned nil while cleanup is still running; UpdateInvite/ApplyUpdate can proceed")}
}

func TestReviewMemberHeartbeatEndsWithConnection(t *testing.T) {
 ctx,cancel:=context.WithCancel(context.Background()); defer cancel()
 ctrl,ctrlPeer:=net.Pipe(); defer ctrl.Close(); defer ctrlPeer.Close()
 member,peer:=net.Pipe(); defer member.Close(); defer peer.Close()
 s:=newClientSession(testTunnel(t),ctrl,"review",nil,Ack{Members:2})
 s.slots[1]=member
 s.dropSlot(1,member)
 done:=make(chan struct{})
 go func(){s.memberHeartbeat(ctx,1);close(done)}()
 select {case <-done: case <-time.After(100*time.Millisecond): t.Fatal("member heartbeat survives removed slot and will target future replacements of that slot")}
}
