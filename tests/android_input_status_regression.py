#!/usr/bin/env python3
"""Run the Android input/telemetry classes and Go routing against API doubles.

Requires JDK and Go; no phone, credentials or root grant is needed.
"""
import pathlib
import subprocess
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
JAVA = ROOT / "node/android/src/main/java/com/ssine/codexnode"
STUBS = {
    "android/os/Looper.java": "package android.os; public class Looper {public static Looper getMainLooper(){return new Looper();}}",
    "android/os/Handler.java": "package android.os; public class Handler {public Handler(Looper l){} public boolean post(Runnable r){r.run();return true;}}",
    "android/os/Bundle.java": "package android.os; public class Bundle {public CharSequence text; public void putCharSequence(String k,CharSequence v){text=v;}}",
    "android/graphics/Path.java": "package android.graphics; public class Path {public void moveTo(float x,float y){} public void lineTo(float x,float y){}}",
    "android/graphics/Rect.java": "package android.graphics; public class Rect {public String toShortString(){return \"[]\";}}",
    "android/view/accessibility/AccessibilityEvent.java": "package android.view.accessibility; public class AccessibilityEvent {}",
    "android/accessibilityservice/GestureDescription.java": "package android.accessibilityservice; public class GestureDescription {public static class StrokeDescription {public StrokeDescription(android.graphics.Path p,long a,long b){}} public static class Builder {public Builder addStroke(StrokeDescription s){return this;} public GestureDescription build(){return new GestureDescription();}}}",
    "android/accessibilityservice/AccessibilityService.java": """
package android.accessibilityservice;
public class AccessibilityService {
    public static final int GLOBAL_ACTION_BACK=1,GLOBAL_ACTION_HOME=2,GLOBAL_ACTION_RECENTS=3,
        GLOBAL_ACTION_NOTIFICATIONS=4,GLOBAL_ACTION_QUICK_SETTINGS=5,GLOBAL_ACTION_POWER_DIALOG=6;
    public android.view.accessibility.AccessibilityNodeInfo root;
    protected void onServiceConnected(){} public void onDestroy(){}
    public void onAccessibilityEvent(android.view.accessibility.AccessibilityEvent e){} public void onInterrupt(){}
    public android.view.accessibility.AccessibilityNodeInfo getRootInActiveWindow(){return root;}
    public boolean performGlobalAction(int a){return true;}
    public boolean dispatchGesture(GestureDescription g,GestureResultCallback c,Object h){c.onCompleted(g);return true;}
    public static class GestureResultCallback {public void onCompleted(GestureDescription d){} public void onCancelled(GestureDescription d){}}
}
""",
    "android/view/accessibility/AccessibilityNodeInfo.java": """
package android.view.accessibility;
public class AccessibilityNodeInfo {
    public static final int FOCUS_INPUT=1,ACTION_SET_TEXT=2097152;
    public static final String ACTION_ARGUMENT_SET_TEXT_CHARSEQUENCE="text";
    public AccessibilityNodeInfo focused; public String text="",pkg="com.example.editor";
    public boolean editable=true,enabled=true,accept=true,throwAction; public int recycles,actions;
    public AccessibilityNodeInfo findFocus(int kind){return focused;}
    public boolean performAction(int action,android.os.Bundle args){
        if(action!=ACTION_SET_TEXT)throw new AssertionError("wrong action");
        actions++;if(throwAction)throw new IllegalStateException("platform action failed");
        if(accept)text=args.text.toString();return accept;
    }
    public void recycle(){recycles++;} public boolean isEditable(){return editable;}
    public boolean isEnabled(){return enabled;} public boolean isClickable(){return false;}
    public CharSequence getPackageName(){return pkg;} public CharSequence getText(){return text;}
    public CharSequence getClassName(){return "EditText";} public CharSequence getContentDescription(){return "";}
    public String getViewIdResourceName(){return "editor";} public void getBoundsInScreen(android.graphics.Rect r){}
    public int getChildCount(){return 0;} public AccessibilityNodeInfo getChild(int i){return null;}
}
""",
    "android/content/Context.java": """
package android.content;
public class Context {
    public Intent battery=new Intent();public boolean denied;
    public Intent registerReceiver(Object r,IntentFilter f){if(denied)throw new SecurityException("denied");return battery;}
    public java.io.File getNoBackupFilesDir(){return new java.io.File("/data/user/0/test/no_backup");}
}
""",
    "android/content/Intent.java": "package android.content; public class Intent {public static final String ACTION_BATTERY_CHANGED=\"battery\";public int level=63,scale=100;public int getIntExtra(String k,int d){return k.equals(\"level\")?level:k.equals(\"scale\")?scale:d;}}",
    "android/content/IntentFilter.java": "package android.content; public class IntentFilter {public IntentFilter(String a){}}",
    "android/os/BatteryManager.java": "package android.os; public class BatteryManager {public static final String EXTRA_LEVEL=\"level\",EXTRA_SCALE=\"scale\",EXTRA_PLUGGED=\"plugged\",EXTRA_STATUS=\"status\";}",
    "android/os/StatFs.java": "package android.os; public class StatFs {public static String path;public static long total=1000,available=400;public StatFs(String p){path=p;}public long getTotalBytes(){return total;}public long getAvailableBytes(){return available;}}",
    "org/json/JSONObject.java": "package org.json; public class JSONObject {public java.util.Map<String,Object> fields=new java.util.HashMap<>();public JSONObject put(String k,Object v){fields.put(k,v);return this;}public Object get(String k){return fields.get(k);}public boolean has(String k){return fields.containsKey(k);}}",
    "org/json/JSONArray.java": "package org.json; public class JSONArray {public java.util.List<Object> values=new java.util.ArrayList<>();public JSONArray put(Object v){values.add(v);return this;}public Object get(int i){return values.get(i);}}",
    "com/ssine/codexnode/NodeConfig.java": "package com.ssine.codexnode; public class NodeConfig {public boolean autoStart;static NodeConfig load(Object c){return new NodeConfig();}static boolean isUserStopped(Object c){return false;}}",
    "com/ssine/codexnode/MiraNodeService.java": "package com.ssine.codexnode;public class MiraNodeService {static boolean isRunning(){return false;}static void start(Object c){}}",
    "com/ssine/codexnode/InputStatusHarness.java": """
package com.ssine.codexnode;
import android.view.accessibility.AccessibilityNodeInfo;
import org.json.*;
public class InputStatusHarness {
    static void check(boolean v,String m){if(!v)throw new AssertionError(m);}
    static void fails(MiraAccessibilityService service,String message) throws Exception {
        try{service.setText("自启动");throw new AssertionError("expected failure");}
        catch(Exception e){check(e.getMessage().contains(message),e.toString());}
    }
    public static void main(String[] args) throws Exception {
        MiraAccessibilityService service=new MiraAccessibilityService();service.onServiceConnected();
        AccessibilityNodeInfo root=new AccessibilityNodeInfo(),field=new AccessibilityNodeInfo();
        root.focused=field;service.root=root;
        for(String text:new String[]{"自启动","abc","Emoji 😀 café","literal %s","line 1\\n第二行"}){
            check(service.setText(text),"accepted");check(field.text.equals(text),"exact Unicode content");
        }
        check(root.recycles==5 && field.recycles==5,"recycle text nodes");
        field.accept=false;fails(service,"rejected_text");field.accept=true;
        field.throwAction=true;fails(service,"platform action failed");field.throwAction=false;
        field.editable=false;fails(service,"editable_field_required");field.editable=true;
        field.enabled=false;fails(service,"editable_field_required");field.enabled=true;
        root.focused=null;fails(service,"editable_field_required");root.focused=field;
        service.root=null;fails(service,"editable_field_required");service.root=root;
        root.focused=root;int recycled=root.recycles;service.setText("self");
        check(root.recycles==recycled+1,"aliased node recycled once");root.focused=field;
        System.out.println("PASS Unicode/ASCII/emoji/literal text, platform failures and focused-field ownership");
        android.content.Context context=new android.content.Context();
        JSONObject status=AndroidStatusReader.read(context);
        check(status.get("battery").toString().contains("level: 63"),"battery API");
        check(status.get("focus").equals("com.example.editor"),"focus package");
        JSONObject disk=(JSONObject)((JSONArray)status.get("disk")).get(0);
        check(disk.get("usagePercent").equals(60.0),"data volume usage");
        check(android.os.StatFs.path.equals("/data/user/0/test/no_backup"),"data volume rather than root image");
        service.onDestroy();context.denied=true;
        status=AndroidStatusReader.read(context);
        check(!status.has("battery") && !status.has("focus"),"unavailable fields omitted");
        check(((JSONObject)status.get("telemetryErrors")).has("battery"),"battery permission reported");
        context.denied=false;context.battery.scale=0;
        android.os.StatFs.total=0;status=AndroidStatusReader.read(context);
        disk=(JSONObject)((JSONArray)status.get("disk")).get(0);
        check(disk.has("error") && !disk.has("usagePercent"),"no fake full disk");
        check(!status.has("battery"),"invalid battery omitted");
        System.out.println("PASS app-safe battery/focus APIs, data volume and unavailable telemetry");
    }
}
""",
}

GO_STUBS = r'''
package node
import("context";"encoding/json";"fmt";"os";"strings";"sync";"time")
type config struct {NodeKey,PrivilegeMode,IdentityFile string}
type capabilityRuntime struct {bridge *androidBridge;configuration config;realRoots,roots []string;processMu sync.Mutex;processes map[string]any}
type displayInfo struct {Width,Height int;SizeOutput,DensityOutput string}
type screenParams struct {Action,Text string;X,Y,StartX,StartY,EndX,EndY,DurationMS *int;KeyCode any}
const maxFileBytes=4*1024*1024
const maxProcessCount=16
var BundledOpenSSH="true"
var uid=0
var calls []string
func effectiveUID()int{return uid}
func commandOutput(ctx context.Context,name string,args ...string)(string,error){
 calls=append(calls,name+" "+strings.Join(args," "))
 if name=="getprop"{return "Android",nil};if name=="wm"{return "Physical size: 100x200",nil};return "",nil
}
func commandOutputLimited(ctx context.Context,n int,name string,args ...string)(string,bool,error){v,e:=commandOutput(ctx,name,args...);return v,false,e}
func parseDisplaySize(string)(int,int){return 100,200}
func platformUptimeSeconds()float64{return 1}
func platformMemoryStatus()any{return map[string]any{"totalBytes":1024}}
func(r *capabilityRuntime)cpuStatus()any{return nil}
func openSSHRuntime()any{return nil}
func systemProcessCount()(int,error){return 1,nil}
func sleepContext(ctx context.Context,d time.Duration)bool{return true}
func usagePercent(a,b int64)float64{return float64(a)/float64(b)*100}
var _=json.Marshal
var _=fmt.Errorf
var _=os.Getpid
'''
GO_TESTS = r'''
package node
import("context";"encoding/json";"net/http";"net/http/httptest";"strings";"testing")
func TestAndroidInputRouting(t *testing.T){
 var text,route string
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  route=r.URL.Path;if r.Header.Get("Authorization")!="Bearer fixture"{t.Error("bridge token missing")}
  var p screenParams;json.NewDecoder(r.Body).Decode(&p);text=p.Text
  w.Header().Set("Content-Type","application/json");w.Write([]byte(`{"accepted":true}`))
 }));defer server.Close()
 runtime:=&capabilityRuntime{bridge:newAndroidBridge(server.URL,"fixture")}
 for _,account:=range []int{0,10000}{uid=account
  for _,value:=range []string{"自启动","😀 café","literal %s","line\ntext"}{
   calls=nil;_,err:=runtime.screen(context.Background(),screenParams{Action:"text",Text:value})
   if err!=nil||text!=value||route!="/v1/screen"||len(calls)!=0{t.Fatalf("unsafe text routing %q: %v %v",value,err,calls)}
  }
 }
 uid=0;calls=nil;_,err:=runtime.screen(context.Background(),screenParams{Action:"text",Text:"abc def"})
 if err!=nil||len(calls)!=1||calls[0]!="input text abc%sdef"{t.Fatalf("ASCII routing: %v %v",calls,err)}
 runtime.bridge=nil;calls=nil
 if _,err=runtime.screen(context.Background(),screenParams{Action:"text",Text:"自启动"});err==nil||len(calls)!=0{t.Fatal("Unicode sent to input shell")}
 for _,value:=range []string{"","nul\x00","\xff",strings.Repeat("a",4097)}{
  if _,err=runtime.screen(context.Background(),screenParams{Action:"text",Text:value});err==nil{t.Fatal("invalid text accepted")}
 }
}
func TestAppTelemetry(t *testing.T){
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  w.Header().Set("Content-Type","application/json")
  if r.URL.Path=="/v1/status"{w.Write([]byte(`{"battery":"level: 63","disk":[{"path":"/data/app","usagePercent":60}],"telemetryErrors":{"focus":"accessibility_permission_required"}}`))}else{w.Write([]byte(`{"width":100,"height":200}`))}
 }));defer server.Close()
 uid=10000;calls=nil
 runtime:=&capabilityRuntime{bridge:newAndroidBridge(server.URL,"fixture"),configuration:config{IdentityFile:t.TempDir()+"/identity.json"}}
 status,err:=runtime.machineStatus(context.Background());if err!=nil{t.Fatal(err)}
 if status["battery"]!="level: 63"{t.Fatal(status)}
 if _,exists:=status["focus"];exists{t.Fatal("invented focus data")}
 for _,call:=range calls{if strings.Contains(call,"dumpsys"){t.Fatal("app performed privileged telemetry",call)}}
 runtime.bridge=nil;calls=nil;status,err=runtime.machineStatus(context.Background());if err!=nil{t.Fatal(err)}
 for _,call:=range calls{if strings.Contains(call,"dumpsys"){t.Fatal("rootless fallback performed privileged telemetry",call)}}
 disk:=status["disk"].([]map[string]any);if disk[0]["path"]=="/"{t.Fatal("read-only root reported as disk")}
}
'''

with tempfile.TemporaryDirectory(prefix="mira-android-input-status-") as temp:
    root = pathlib.Path(temp)
    for name, source in STUBS.items():
        target = root / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(source)
    sources = [str(p) for p in root.rglob("*.java")]
    sources += [str(JAVA / name) for name in ["MiraAccessibilityService.java", "AndroidStatusReader.java"]]
    subprocess.run(["javac", "-d", str(root), *sources], check=True)
    subprocess.run(["java", "-cp", str(root), "com.ssine.codexnode.InputStatusHarness"], check=True, timeout=20)
    go = root / "go"
    go.mkdir()
    (go / "go.mod").write_text("module mira-android-regression\n\ngo 1.26.6\n")
    for name in ["screen.go", "platform_android.go", "bridge.go", "disk_unix.go"]:
        source = (ROOT / "node/internal" / name).read_text()
        source = source.replace("//go:build android\n", "").replace("//go:build !windows\n", "")
        source = source.replace("os.Geteuid()", "effectiveUID()")
        (go / ("fixture_" + name.replace("_android", "").replace("_unix", ""))).write_text(source)
    (go / "stubs.go").write_text(GO_STUBS)
    (go / "regression_test.go").write_text(GO_TESTS)
    subprocess.run(["go", "test", "-v", "./..."], cwd=go, check=True, timeout=60)
