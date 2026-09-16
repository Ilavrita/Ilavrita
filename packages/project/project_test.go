package project

import (
	"errors"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

func TestValidateIDRejectsEverySentinel(t *testing.T) {
	tests := []struct {
		name  string
		id    ID
		valid bool
	}{
		{name: "empty", id: "", valid: false},
		{name: "wildcard", id: "*", valid: false},
		{name: "ordinary project", id: "prj_a", valid: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateID(tc.id)

			if tc.valid && err != nil {
				t.Fatalf("ValidateID(%q) = %v, want nil", tc.id, err)
			}

			if !tc.valid && !errors.Is(err, ErrInvalidProjectID) {
				t.Fatalf("ValidateID(%q) = %v, want ErrInvalidProjectID", tc.id, err)
			}
		})
	}
}

func TestKindAllowsSuperAdminOnlyInTheSuperProject(t *testing.T) {
	tests := []struct {
		name  string
		kind  Kind
		valid bool
		super bool
	}{
		{name: "standard", kind: KindStandard, valid: true, super: false},
		{name: "super", kind: KindSuper, valid: true, super: true},
		{name: "unknown", kind: "root", valid: false, super: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.kind.Valid() != tc.valid {
				t.Errorf("Valid() = %v, want %v", tc.kind.Valid(), tc.valid)
			}

			if tc.kind.AllowsSuperAdmin() != tc.super {
				t.Errorf("AllowsSuperAdmin() = %v, want %v", tc.kind.AllowsSuperAdmin(), tc.super)
			}
		})
	}
}

func TestProjectStateTransitions(t *testing.T) {
	tests := []struct {
		name  string
		from  State
		to    State
		allow bool
	}{
		{name: "active suspends", from: StateActive, to: StateSuspended, allow: true},
		{name: "active archives", from: StateActive, to: StateArchived, allow: true},
		{name: "active deletes", from: StateActive, to: StateDeleting, allow: true},
		{name: "suspended reactivates", from: StateSuspended, to: StateActive, allow: true},
		{name: "suspended archives", from: StateSuspended, to: StateArchived, allow: true},
		{name: "suspended deletes", from: StateSuspended, to: StateDeleting, allow: true},
		{name: "archived reactivates", from: StateArchived, to: StateActive, allow: true},
		{name: "archived deletes", from: StateArchived, to: StateDeleting, allow: true},
		{name: "deleting never reactivates", from: StateDeleting, to: StateActive, allow: false},
		{name: "deleting never suspends", from: StateDeleting, to: StateSuspended, allow: false},
		{name: "deleting never archives", from: StateDeleting, to: StateArchived, allow: false},
		{name: "no self transition", from: StateActive, to: StateActive, allow: false},
		{name: "no unknown target", from: StateActive, to: "purged", allow: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.from.CanTransitionTo(tc.to) != tc.allow {
				t.Fatalf("CanTransitionTo(%s to %s) = %v, want %v", tc.from, tc.to, !tc.allow, tc.allow)
			}

			next, err := tc.from.TransitionTo(tc.to)
			switch {
			case tc.allow && err != nil:
				t.Fatalf("TransitionTo(%s to %s) = %v, want nil", tc.from, tc.to, err)
			case tc.allow && next != tc.to:
				t.Fatalf("TransitionTo(%s to %s) = %s", tc.from, tc.to, next)
			case !tc.allow && !errors.Is(err, ErrInvalidTransition):
				t.Fatalf("TransitionTo(%s to %s) = %v, want ErrInvalidTransition", tc.from, tc.to, err)
			}
		})
	}
}

func TestTransitionFromAnUnknownStateFailsClosed(t *testing.T) {
	var unknown State = "quarantined"

	if _, err := unknown.TransitionTo(StateActive); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("TransitionTo from an unknown state = %v, want ErrUnknownState", err)
	}
}

func TestProjectStateAdmission(t *testing.T) {
	actions := []storage.Action{
		storage.ActionRead, storage.ActionSearch, storage.ActionHistory,
		storage.ActionWrite, storage.ActionDelete,
	}

	tests := []struct {
		name    string
		state   State
		admits  map[storage.Action]bool
		comment string
	}{
		{
			name:  "active admits everything",
			state: StateActive,
			admits: map[storage.Action]bool{
				storage.ActionRead: true, storage.ActionSearch: true, storage.ActionHistory: true,
				storage.ActionWrite: true, storage.ActionDelete: true,
			},
		},
		{
			name:   "suspended admits nothing",
			state:  StateSuspended,
			admits: map[storage.Action]bool{},
		},
		{
			name:  "archived admits reads only",
			state: StateArchived,
			admits: map[storage.Action]bool{
				storage.ActionRead: true, storage.ActionSearch: true, storage.ActionHistory: true,
			},
		},
		{
			name:   "deleting admits nothing",
			state:  StateDeleting,
			admits: map[storage.Action]bool{},
		},
		{
			name:   "an unknown state admits nothing",
			state:  "quarantined",
			admits: map[storage.Action]bool{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, action := range actions {
				if got := tc.state.Admits(action); got != tc.admits[action] {
					t.Errorf("%s admits %s = %v, want %v", tc.state, action, got, tc.admits[action])
				}
			}
		})
	}
}
