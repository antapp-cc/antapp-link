package proto
import "testing"
func TestReviewAllMembersLostMustUseControl(t *testing.T){
 ft:=NewFlowTable()
 p:=buildTCP([4]byte{10,10,0,2},[4]byte{8,8,8,8},40000,443)
 got:=ft.Slot(p,4,func(int)bool{return false})
 if got!=0 {t.Fatalf("all member slots absent, control alive: chose missing slot %d instead of control slot 0",got)}
}
func TestReviewExistingFlowNeedsDeadSlotRecovery(t *testing.T){
 ft:=NewFlowTable()
 p:=buildTCP([4]byte{10,10,0,2},[4]byte{8,8,8,8},40000,443)
 first:=ft.Slot(p,4,func(int)bool{return true})
 got:=ft.Slot(p,4,func(k int)bool{return k!=first})
 if got==first {t.Fatalf("flow stays pinned to dead slot %d although other slots are live",got)}
}
