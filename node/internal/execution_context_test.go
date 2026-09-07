package node

import "testing"

func TestValidateExecutionRequest(t *testing.T) {
	sessionID := uint32(7)
	for _, request := range []executionRequest{
		{}, {Context: executionContextUser}, {Context: executionContextSystem},
		{Context: executionContextUser, UserSessionID: &sessionID}, {UserSessionID: &sessionID},
	} {
		if err := validateExecutionRequest(request); err != nil {
			t.Fatalf("valid execution request %#v: %v", request, err)
		}
	}
	for _, request := range []executionRequest{
		{Context: "administrator"}, {Context: executionContextSystem, UserSessionID: &sessionID},
	} {
		if err := validateExecutionRequest(request); err == nil {
			t.Fatalf("invalid execution request accepted: %#v", request)
		}
	}
}

func TestMergeExecutionEnvironment(t *testing.T) {
	actual := mergeExecutionEnvironment(
		[]string{"Path=base", "KEEP=value"},
		map[string]string{"PATH": "user", "NEW": "added"},
		true,
	)
	expected := map[string]string{"PATH": "user", "KEEP": "value", "NEW": "added"}
	for _, item := range actual {
		for name, value := range expected {
			if item == name+"="+value || (name == "PATH" && item == "Path="+value) {
				delete(expected, name)
			}
		}
	}
	if len(expected) != 0 {
		t.Fatalf("environment overrides missing: %#v from %#v", expected, actual)
	}
}
