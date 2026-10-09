package fsperm

import (
	"errors"
	"testing"
)

func TestCheckOwnerOnlyACL(t *testing.T) {
	const (
		ownerSID = "S-1-5-21-100-200-300-1001"
		otherSID = "S-1-5-21-100-200-300-1002"
	)

	tests := []struct {
		name        string
		ownerSID    string
		processSID  string
		daclPresent bool
		daclNull    bool
		entries     []aclEntry
		want        error
	}{
		{
			name:        "owner allow",
			ownerSID:    ownerSID,
			processSID:  ownerSID,
			daclPresent: true,
			entries:     []aclEntry{{kind: aclACEAllowed, trusteeSID: ownerSID, accessMask: 0x001f01ff}},
		},
		{
			name:        "other deny and owner allow",
			ownerSID:    ownerSID,
			processSID:  ownerSID,
			daclPresent: true,
			entries: []aclEntry{
				{kind: aclACEDenied, trusteeSID: otherSID, accessMask: 0x001f01ff},
				{kind: aclACEAllowed, trusteeSID: ownerSID, accessMask: 0x001f01ff},
			},
		},
		{
			name:        "zero mask does not grant access",
			ownerSID:    ownerSID,
			processSID:  ownerSID,
			daclPresent: true,
			entries:     []aclEntry{{kind: aclACEAllowed, trusteeSID: otherSID, accessMask: 0}},
		},
		{
			name:        "different owner",
			ownerSID:    otherSID,
			processSID:  ownerSID,
			daclPresent: true,
			want:        ErrNotOwnerOnly,
		},
		{
			name:       "missing DACL",
			ownerSID:   ownerSID,
			processSID: ownerSID,
			want:       ErrNotOwnerOnly,
		},
		{
			name:        "null DACL",
			ownerSID:    ownerSID,
			processSID:  ownerSID,
			daclPresent: true,
			daclNull:    true,
			want:        ErrNotOwnerOnly,
		},
		{
			name:        "Everyone allow",
			ownerSID:    ownerSID,
			processSID:  ownerSID,
			daclPresent: true,
			entries:     []aclEntry{{kind: aclACEAllowed, trusteeSID: "S-1-1-0", accessMask: 1}},
			want:        ErrNotOwnerOnly,
		},
		{
			name:        "SYSTEM allow is not owner-only",
			ownerSID:    ownerSID,
			processSID:  ownerSID,
			daclPresent: true,
			entries:     []aclEntry{{kind: aclACEAllowed, trusteeSID: "S-1-5-18", accessMask: 1}},
			want:        ErrNotOwnerOnly,
		},
		{
			name:        "Administrators allow is not owner-only",
			ownerSID:    ownerSID,
			processSID:  ownerSID,
			daclPresent: true,
			entries:     []aclEntry{{kind: aclACEAllowed, trusteeSID: "S-1-5-32-544", accessMask: 1}},
			want:        ErrNotOwnerOnly,
		},
		{
			name:        "unknown ACE",
			ownerSID:    ownerSID,
			processSID:  ownerSID,
			daclPresent: true,
			entries:     []aclEntry{{kind: aclACEUnknown}},
			want:        ErrUnverifiable,
		},
		{
			name:        "missing owner SID",
			processSID:  ownerSID,
			daclPresent: true,
			want:        ErrUnverifiable,
		},
		{
			name:        "missing allow trustee SID",
			ownerSID:    ownerSID,
			processSID:  ownerSID,
			daclPresent: true,
			entries:     []aclEntry{{kind: aclACEAllowed, accessMask: 1}},
			want:        ErrUnverifiable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkOwnerOnlyACL(tt.ownerSID, tt.processSID, tt.daclPresent, tt.daclNull, tt.entries)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want errors.Is(_, %v)", err, tt.want)
			}
		})
	}
}

func TestCheckProcessOwnerDefault(t *testing.T) {
	const userSID = "S-1-5-21-100-200-300-1001"
	tests := []struct {
		name     string
		userSID  string
		ownerSID string
		want     error
	}{
		{name: "matches user", userSID: userSID, ownerSID: userSID},
		{name: "admin default owner differs", userSID: userSID, ownerSID: "S-1-5-32-544", want: ErrNotOwnerOnly},
		{name: "user is unverifiable", ownerSID: userSID, want: ErrUnverifiable},
		{name: "owner is unverifiable", userSID: userSID, want: ErrUnverifiable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkProcessOwnerDefault(tt.userSID, tt.ownerSID)
			if tt.want == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want errors.Is(_, %v)", err, tt.want)
			}
		})
	}
}

func TestCheckPlainWindowsKind(t *testing.T) {
	tests := []struct {
		name         string
		wantDir      bool
		isDir        bool
		isReparse    bool
		isDisk       bool
		wantAccepted bool
	}{
		{name: "plain file", isDisk: true, wantAccepted: true},
		{name: "plain directory", wantDir: true, isDir: true, isDisk: true, wantAccepted: true},
		{name: "file requested as directory", wantDir: true, isDisk: true},
		{name: "directory requested as file", isDir: true, isDisk: true},
		{name: "reparse file", isReparse: true, isDisk: true},
		{name: "non-disk object", isDisk: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPlainWindowsKind(tt.wantDir, tt.isDir, tt.isReparse, tt.isDisk)
			if tt.wantAccepted && err != nil {
				t.Fatalf("plain object rejected: %v", err)
			}
			if !tt.wantAccepted && !errors.Is(err, ErrNotOwnerOnly) {
				t.Fatalf("error = %v, want ErrNotOwnerOnly", err)
			}
		})
	}
}

func TestCheckOwnerOnlyDirInheritance(t *testing.T) {
	const ownerSID = "S-1-5-21-100-200-300-1001"
	const otherSID = "S-1-5-21-100-200-300-1002"
	tests := []struct {
		name    string
		entries []aclEntry
		want    error
	}{
		{
			name:    "owner full control inherits to files and directories",
			entries: []aclEntry{{kind: aclACEAllowed, trusteeSID: ownerSID, accessMask: 0x001f01ff, aceFlags: 0x03}},
		},
		{
			name:    "generic all inherits to files and directories",
			entries: []aclEntry{{kind: aclACEAllowed, trusteeSID: ownerSID, accessMask: 0x10000000, aceFlags: 0x03}},
		},
		{
			name:    "file inheritance only is insufficient",
			entries: []aclEntry{{kind: aclACEAllowed, trusteeSID: ownerSID, accessMask: 0x001f01ff, aceFlags: 0x01}},
			want:    ErrNotOwnerOnly,
		},
		{
			name:    "directory inheritance only is insufficient",
			entries: []aclEntry{{kind: aclACEAllowed, trusteeSID: ownerSID, accessMask: 0x001f01ff, aceFlags: 0x02}},
			want:    ErrNotOwnerOnly,
		},
		{
			name:    "other user inheritance is insufficient",
			entries: []aclEntry{{kind: aclACEAllowed, trusteeSID: otherSID, accessMask: 0x001f01ff, aceFlags: 0x03}},
			want:    ErrNotOwnerOnly,
		},
		{
			name:    "read-only owner inheritance is insufficient",
			entries: []aclEntry{{kind: aclACEAllowed, trusteeSID: ownerSID, accessMask: 1, aceFlags: 0x03}},
			want:    ErrNotOwnerOnly,
		},
		{
			name:    "no-propagate owner ACE is insufficient",
			entries: []aclEntry{{kind: aclACEAllowed, trusteeSID: ownerSID, accessMask: 0x001f01ff, aceFlags: 0x07}},
			want:    ErrNotOwnerOnly,
		},
		{
			name: "propagating deny ACE can conflict with inherited owner access",
			entries: []aclEntry{
				{kind: aclACEAllowed, trusteeSID: ownerSID, accessMask: 0x001f01ff, aceFlags: 0x03},
				{kind: aclACEDenied, trusteeSID: otherSID, accessMask: 1, aceFlags: 0x01},
			},
			want: ErrNotOwnerOnly,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkOwnerOnlyDirInheritance(ownerSID, tt.entries, true)
			if tt.want == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want errors.Is(_, %v)", err, tt.want)
			}
		})
	}
	if err := checkOwnerOnlyDirInheritance(ownerSID, []aclEntry{{
		kind: aclACEAllowed, trusteeSID: ownerSID, accessMask: 0x001f01ff, aceFlags: 0x03,
	}}, false); !errors.Is(err, ErrNotOwnerOnly) {
		t.Fatalf("an unprotected DACL was accepted: %v", err)
	}
}

func TestPrivateSecurityDescriptorTemplates(t *testing.T) {
	const sid = "S-1-5-21-100-200-300-1001"
	if got, want := privateDirectorySDDL(sid), "O:"+sid+"D:P(A;OICI;FA;;;"+sid+")"; got != want {
		t.Fatalf("directory SDDL = %q, want %q", got, want)
	}
	if got, want := privateFileSDDL(sid), "O:"+sid+"D:P(A;;FA;;;"+sid+")"; got != want {
		t.Fatalf("file SDDL = %q, want %q", got, want)
	}
}
