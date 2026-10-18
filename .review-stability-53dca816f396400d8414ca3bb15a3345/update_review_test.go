package update
import("errors";"os";"path/filepath";"testing")
func TestReviewLaunchFailureMustRestoreExecutable(t *testing.T){
 dir:=t.TempDir(); self:=filepath.Join(dir,"app.exe"); next:=filepath.Join(dir,"new.exe")
 if err:=os.WriteFile(self,[]byte("working-old"),0600);err!=nil {t.Fatal(err)}
 if err:=os.WriteFile(next,[]byte("broken-new"),0600);err!=nil {t.Fatal(err)}
 err:=swapExe(self,next,func(string)error{return errors.New("simulated launch failure")})
 if err==nil {t.Fatal("expected launch error")}
 got,rerr:=os.ReadFile(self);if rerr!=nil{t.Fatal(rerr)}
 if string(got)!="working-old" {t.Fatalf("launch failed but normal launch path still contains %q; old version only survives as .old",got)}
}
