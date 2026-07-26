package service

import "testing"

func TestDefaultUserCanSyncExternalChatHistory(t *testing.T) {
	permissions := DefaultPermissionSetForRole(AuthRoleUser)
	for _, test := range []struct {
		method string
		path   string
	}{
		{method: "GET", path: "/api/external-chat-conversations"},
		{method: "PUT", path: "/api/external-chat-conversations"},
	} {
		if !HasAPIPermission(permissions, test.method, test.path) {
			t.Fatalf("default user missing %s %s", test.method, test.path)
		}
	}
}

func TestImageConversationHistoryIsNotRoleGated(t *testing.T) {
	// Catalog no longer exposes image conversation history; access is granted to every
	// authenticated identity via isPermissionCheckSkipped in the HTTP layer.
	for _, key := range []string{
		APIPermissionKey("GET", "/api/image-conversations"),
		APIPermissionKey("PUT", "/api/image-conversations"),
	} {
		for _, permission := range AllAPIPermissions() {
			if permission.Key == key {
				t.Fatalf("image conversation history should not be in RBAC catalog: %s", key)
			}
		}
	}
	if HasAPIPermission(DefaultPermissionSetForRole(AuthRoleUser), "GET", "/api/image-conversations") {
		t.Fatal("default user role set should not claim image conversation API keys")
	}
}

func TestMergeDefaultManagedRoleAddsHistoryPermissionsOnlyToBuiltinRole(t *testing.T) {
	oldPermissions := []string{
		APIPermissionKey("GET", "/api/external-chat-providers"),
		APIPermissionKey("POST", "/api/external-chat/completions"),
	}
	roles := mergeDefaultManagedRole([]ManagedRole{
		{ID: DefaultManagedRoleID, Name: "普通用户", MenuPaths: []string{"/external-chat"}, APIPermissions: oldPermissions},
		{ID: "custom-chat", Name: "自定义聊天", MenuPaths: []string{"/external-chat"}, APIPermissions: oldPermissions},
	})

	for _, role := range roles {
		permissions := role.PermissionSet()
		hasChatRead := HasAPIPermission(permissions, "GET", "/api/external-chat-conversations")
		hasChatWrite := HasAPIPermission(permissions, "PUT", "/api/external-chat-conversations")
		switch role.ID {
		case DefaultManagedRoleID:
			if !hasChatRead || !hasChatWrite {
				t.Fatalf("builtin role missing history permissions: %#v", role.APIPermissions)
			}
		case "custom-chat":
			if hasChatRead || hasChatWrite {
				t.Fatalf("custom role was implicitly expanded: %#v", role.APIPermissions)
			}
		}
	}
}

func TestNormalizeAPIPermissionsMigratesCreationTaskPermissions(t *testing.T) {
	permissions := NormalizeAPIPermissions([]string{
		APIPermissionKey("GET", "/api/image-tasks"),
		"POST /api/image-tasks",
	})

	if !HasAPIPermission(PermissionSet{APIPermissions: permissions}, "GET", "/api/creation-tasks") {
		t.Fatalf("migrated permissions missing creation task read: %#v", permissions)
	}
	if !HasAPIPermission(PermissionSet{APIPermissions: permissions}, "POST", "/api/creation-tasks/chat-completions") {
		t.Fatalf("migrated permissions missing creation task submit subtree: %#v", permissions)
	}
	if HasAPIPermission(PermissionSet{APIPermissions: permissions}, "GET", "/api/image-tasks") {
		t.Fatalf("old image task route should not be authorized: %#v", permissions)
	}
}

func TestAPIPermissionsAccountPoolAreExplicit(t *testing.T) {
	readOnly := PermissionSet{APIPermissions: []string{APIPermissionKey("GET", "/api/accounts")}}
	if !HasAPIPermission(readOnly, "GET", "/api/accounts") {
		t.Fatalf("read-only account permission missing account list")
	}
	if HasAPIPermission(readOnly, "GET", "/api/accounts/tokens") {
		t.Fatalf("account list permission should not allow token export")
	}
	if HasAPIPermission(readOnly, "POST", "/api/accounts/refresh") {
		t.Fatalf("account list permission should not allow refresh")
	}
	if HasAPIPermission(readOnly, "POST", "/api/accounts/upstream-actions") {
		t.Fatalf("account list permission should not allow upstream actions")
	}
	if HasAPIPermission(readOnly, "POST", "/api/accounts/toggle-enabled") {
		t.Fatalf("account list permission should not allow toggling enabled state")
	}

	operators := PermissionSet{APIPermissions: NormalizeAPIPermissions([]string{
		APIPermissionKey("GET", "/api/accounts/tokens"),
		APIPermissionKey("POST", "/api/accounts"),
		APIPermissionKey("POST", "/api/accounts/session"),
		APIPermissionKey("POST", "/api/accounts/refresh"),
		APIPermissionKey("POST", "/api/accounts/upstream-actions"),
		APIPermissionKey("POST", "/api/accounts/update"),
		APIPermissionKey("POST", "/api/accounts/toggle-enabled"),
		APIPermissionKey("DELETE", "/api/accounts"),
	})}
	for _, tc := range []struct {
		method string
		path   string
	}{
		{"GET", "/api/accounts/tokens"},
		{"POST", "/api/accounts"},
		{"POST", "/api/accounts/session"},
		{"POST", "/api/accounts/refresh"},
		{"POST", "/api/accounts/upstream-actions"},
		{"POST", "/api/accounts/update"},
		{"POST", "/api/accounts/toggle-enabled"},
		{"DELETE", "/api/accounts"},
	} {
		if !HasAPIPermission(operators, tc.method, tc.path) {
			t.Fatalf("missing explicit permission for %s %s in %#v", tc.method, tc.path, operators.APIPermissions)
		}
	}
}
