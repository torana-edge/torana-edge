package wasm
import ("context";"encoding/json";"os";"testing")

func TestDogfood_WASM(t *testing.T) {
	b,_:=os.ReadFile("../../plugins/delegator/plugin.wasm")
	r:=NewRuntime(context.Background());defer r.Close()
	p,_:=r.LoadPlugin("delegator",b)
	
	input:=map[string]any{"test":"hello wasm!"}
	var output map[string]any
	if err:=p.CallRequest(context.Background(),"on_chat_request",input,&output);err!=nil{t.Fatal(err)}
	
	b2,_:=json.Marshal(output)
	t.Logf("output: %s",b2)
	if output["status"]!="ok"{t.Errorf("expected status=ok, got %v",output["status"])}
	if output["echo"]==nil{t.Error("expected echo")}
	t.Log("✅ WASM plugin round-trip works!")
}
