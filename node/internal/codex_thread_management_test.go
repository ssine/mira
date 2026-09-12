package node

import "testing"

func TestThreadHandoffDoesNotBlockOtherConversations(t *testing.T) {
	manager := newAppServerManager(config{})
	manager.activeThreads = map[string]bool{"other": true, "root": true} // root can be stale
	if err := manager.beginThreadManagement("handoff", []string{"root", "child"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"root", "child"} {
		if err := manager.reserveAccountRequest("browser", []byte(`{"id":1,"method":"turn/start","params":{"threadId":"`+id+`"}}`)); err == nil {
			t.Fatal("handoff admitted a competing turn")
		}
	}
	if err := manager.reserveAccountRequest("browser", []byte(`{"id":2,"method":"turn/start","params":{"threadId":"other"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := manager.reserveAccountRequest("handoff", []byte(`{"id":3,"method":"mira/thread/unload","params":{"threadId":"root","threadIds":["root","child"]}}`)); err != nil {
		t.Fatal(err)
	}
	if err := manager.reserveAccountRequest("handoff", []byte(`{"id":4,"method":"mira/thread/unload","params":{"threadId":"root","threadIds":["other"]}}`)); err == nil {
		t.Fatal("unload escaped its scope")
	}
	if err := manager.beginAccountManagement("login"); err == nil {
		t.Fatal("credential mutation overlapped handoff")
	}
	manager.endThreadManagement("handoff")
	if err := manager.reserveAccountRequest("browser", []byte(`{"id":5,"method":"thread/resume","params":{"threadId":"root"}}`)); err != nil {
		t.Fatal(err)
	}
}

func TestThreadHandoffRejectsPendingRequestsInItsTree(t *testing.T) {
	manager := newAppServerManager(config{})
	request := []byte(`{"id":1,"method":"thread/resume","params":{"threadId":"child"}}`)
	if err := manager.reserveAccountRequest("browser", request); err != nil {
		t.Fatal(err)
	}
	if err := manager.beginThreadManagement("handoff", []string{"root", "child"}); err == nil {
		t.Fatal("handoff overlapped pending resume")
	}
	if len(manager.threadManagement) != 0 {
		t.Fatal("failed handoff leaked gates")
	}
	manager.observeAccountResponse("browser", []byte(`{"id":1,"result":{}}`))
	if err := manager.beginThreadManagement("handoff", []string{"root", "child"}); err != nil {
		t.Fatal(err)
	}
	manager.endThreadManagement("unrelated")
	if len(manager.threadManagement) != 2 {
		t.Fatal("unrelated close released handoff")
	}
	manager.endThreadManagement("handoff")
	if len(manager.threadManagement) != 0 {
		t.Fatal("closed handoff leaked gates")
	}
}
