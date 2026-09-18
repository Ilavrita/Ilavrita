package pocketbase

import (
	"bytes"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

// claimedAt is the instant the bootstrap tests decide at, so a claim's whole life
// is stated rather than measured against a clock the test cannot hold still.
var claimedAt = time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)

func newProjectStore(t *testing.T) (*ProjectStore, *sql.DB) {
	t.Helper()

	_, db := newStore(t)

	return NewProjectStore(db), db
}

// bootstrapper wires a deterministic one: a fixed clock and an entropy source
// that never runs short, so a token is minted whole every time.
func bootstrapper(t *testing.T, store *ProjectStore) *project.Bootstrapper {
	t.Helper()

	return project.NewBootstrapper(store,
		func() time.Time { return claimedAt },
		bytes.NewReader(bytes.Repeat([]byte{'e'}, 1024)))
}

func superProject(t *testing.T) project.SuperProject {
	t.Helper()

	super, err := project.NewSuperProject("prj_super", "super", "Super Project")
	if err != nil {
		t.Fatalf("NewSuperProject: %v", err)
	}

	return super
}

// seedFounder writes the identity the claim mints a Super Admin for. A membership
// names a user that must exist, because user_id carries a foreign key.
func seedFounder(t *testing.T, db *sql.DB) {
	t.Helper()

	execAll(t, db, []string{
		"INSERT INTO users (id, scope, home_project_id, email_normalized, email_display," +
			" password_hash, state, created_at, updated_at)" +
			" VALUES ('usr_founder', 'server', NULL, 'founder@example.test', 'founder@example.test'," +
			" 'argon2id$hash', 'active', 0, 0)",
	})
}

// TestAnInstallProvisionsItsSuperProjectAndOneClaimToken. Before this store
// existed the bootstrapper had no implementation to write through, so neither the
// Super Project nor its install record could come into existence at all.
func TestAnInstallProvisionsItsSuperProjectAndOneClaimToken(t *testing.T) {
	store, _ := newProjectStore(t)

	provisioning, err := bootstrapper(t, store).Provision(t.Context(), project.ProvisionConfig{
		SuperProject: superProject(t), TokenTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if !provisioning.Created || provisioning.Token.Reveal() == "" {
		t.Fatalf("provisioning created %t with token %q", provisioning.Created, provisioning.Token)
	}

	written, version, found, err := store.ByID(t.Context(), "prj_super")
	if err != nil || !found {
		t.Fatalf("read the super project: found %t, err %v", found, err)
	}

	if written.Kind() != project.KindSuper || version != 1 {
		t.Errorf("the super project reads %v at version %d", written, version)
	}

	// The Project that administers the install never stores patient data.
	if written.AllowsClinicalData() {
		t.Error("the super project allows clinical data")
	}
}

// TestASecondProvisioningRunIssuesNoSecondToken, so a restart re-arms nothing.
func TestASecondProvisioningRunIssuesNoSecondToken(t *testing.T) {
	store, _ := newProjectStore(t)
	cfg := project.ProvisionConfig{SuperProject: superProject(t), TokenTTL: time.Hour}

	if _, err := bootstrapper(t, store).Provision(t.Context(), cfg); err != nil {
		t.Fatalf("first run: %v", err)
	}

	second, err := bootstrapper(t, store).Provision(t.Context(), cfg)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	if second.Created {
		t.Error("a second run created a second install")
	}

	if second.Token.Reveal() != "" {
		t.Error("a second run issued a second token")
	}
}

// TestSpendingTheClaimMintsTheFirstSuperAdmin is the whole point of the bootstrap:
// standing in the Super Project comes only from this one write.
func TestSpendingTheClaimMintsTheFirstSuperAdmin(t *testing.T) {
	store, db := newProjectStore(t)
	seedFounder(t, db)

	boot := bootstrapper(t, store)

	provisioning, err := boot.Provision(t.Context(), project.ProvisionConfig{
		SuperProject: superProject(t), TokenTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	founder := project.PrincipalRef{Kind: project.PrincipalUser, ID: "usr_founder"}

	member, err := boot.Claim(t.Context(), provisioning.Token, founder)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if !member.IsSuperAdmin() {
		t.Fatalf("the claim minted %v, which holds no super admin standing", member)
	}

	// The membership is readable through the resolver every authorization decision
	// uses, which is the difference between a row and standing.
	resolved, found, err := NewMembershipResolver(db).Membership(t.Context(), "prj_super", founder)
	if err != nil || !found {
		t.Fatalf("resolve the minted membership: found %t, err %v", found, err)
	}

	if !resolved.IsSuperAdmin() || !resolved.HoldsStanding() {
		t.Errorf("the resolved membership holds %v", resolved)
	}

	// The install is spent, so the same token confers nothing a second time.
	if _, err := boot.Claim(t.Context(), provisioning.Token, founder); !errors.Is(
		err, project.ErrBootstrapComplete) {
		t.Errorf("replaying the claim: got %v, want ErrBootstrapComplete", err)
	}
}

// TestAFailedClaimLeavesTheInstallClaimable. The membership write and the spend
// are one transaction, so a refused claim spends nothing.
func TestAFailedClaimLeavesTheInstallClaimable(t *testing.T) {
	store, db := newProjectStore(t)
	boot := bootstrapper(t, store)

	provisioning, err := boot.Provision(t.Context(), project.ProvisionConfig{
		SuperProject: superProject(t), TokenTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// No users row exists, so the membership's foreign key refuses the insert and
	// the spend must roll back with it.
	ghost := project.PrincipalRef{Kind: project.PrincipalUser, ID: "usr_ghost"}
	if _, err := boot.Claim(t.Context(), provisioning.Token, ghost); err == nil {
		t.Fatal("a claim naming no identity succeeded")
	}

	instance, found, err := store.Instance(t.Context())
	if err != nil || !found {
		t.Fatalf("read the install: found %t, err %v", found, err)
	}

	if instance.IsComplete() {
		t.Error("a failed claim spent the install")
	}

	// The real founder can still claim it.
	seedFounder(t, db)

	founder := project.PrincipalRef{Kind: project.PrincipalUser, ID: "usr_founder"}
	if _, err := boot.Claim(t.Context(), provisioning.Token, founder); err != nil {
		t.Errorf("the install was left unclaimable: %v", err)
	}
}

// TestAMachinePrincipalCannotClaimTheInstall. Administering the install is
// answerable work, and the claim refuses before it spends anything.
func TestAMachinePrincipalCannotClaimTheInstall(t *testing.T) {
	store, _ := newProjectStore(t)
	boot := bootstrapper(t, store)

	provisioning, err := boot.Provision(t.Context(), project.ProvisionConfig{
		SuperProject: superProject(t), TokenTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	machine := project.PrincipalRef{Kind: project.PrincipalClientApplication, ID: "cli_loader"}
	if _, err := boot.Claim(t.Context(), provisioning.Token, machine); !errors.Is(
		err, project.ErrMachinePrincipalPrivilege) {
		t.Fatalf("got %v, want ErrMachinePrincipalPrivilege", err)
	}

	instance, _, err := store.Instance(t.Context())
	if err != nil {
		t.Fatalf("read the install: %v", err)
	}

	if instance.IsComplete() {
		t.Error("a refused claim spent the install")
	}
}

// TestAProjectRoundTripsAndResolvesByItsSlug. A request names a Project by slug,
// so the two lookups must agree.
func TestAProjectRoundTripsAndResolvesByItsSlug(t *testing.T) {
	store, _ := newProjectStore(t)

	clinic, err := project.NewProject(project.Config{
		ID: "prj_clinic", Slug: "clinic", Name: "Clinic A",
		State: project.StateActive, AllowClinicalData: true,
	})
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	if _, err := store.Create(t.Context(), clinic); err != nil {
		t.Fatalf("Create: %v", err)
	}

	byID, _, found, err := store.ByID(t.Context(), "prj_clinic")
	if err != nil || !found {
		t.Fatalf("ByID: found %t, err %v", found, err)
	}

	bySlug, _, found, err := store.BySlug(t.Context(), "clinic")
	if err != nil || !found {
		t.Fatalf("BySlug: found %t, err %v", found, err)
	}

	if byID.ID() != bySlug.ID() || byID.Name() != "Clinic A" || byID.Environment() != "production" {
		t.Errorf("the two lookups disagree: %v and %v", byID, bySlug)
	}
}

// TestASecondProjectCannotTakeASlugTheInstallResolves, because a slug decides
// which Project a request means.
func TestASecondProjectCannotTakeASlugTheInstallResolves(t *testing.T) {
	store, _ := newProjectStore(t)

	first, err := project.NewProject(project.Config{
		ID: "prj_one", Slug: "clinic", Name: "Clinic A", State: project.StateActive,
	})
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	if _, err := store.Create(t.Context(), first); err != nil {
		t.Fatalf("Create: %v", err)
	}

	second, err := project.NewProject(project.Config{
		ID: "prj_two", Slug: "clinic", Name: "Clinic B", State: project.StateActive,
	})
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	if _, err := store.Create(t.Context(), second); !errors.Is(err, ErrProjectSlugTaken) {
		t.Errorf("got %v, want ErrProjectSlugTaken", err)
	}

	if _, _, found, err := store.ByID(t.Context(), "prj_two"); err != nil || found {
		t.Errorf("a refused create left a row: found %t, err %v", found, err)
	}
}

// TestTheSuperProjectIsNotCreatableThroughTheOrdinaryPath. It is provisioned once,
// with its install record, so there is no second way to bring one into existence.
func TestTheSuperProjectIsNotCreatableThroughTheOrdinaryPath(t *testing.T) {
	store, _ := newProjectStore(t)

	super, err := project.NewSuperProjectRecord(project.Config{
		ID: "prj_super", Slug: "super", Name: "Super Project", State: project.StateActive,
	})
	if err != nil {
		t.Fatalf("NewSuperProjectRecord: %v", err)
	}

	if _, err := store.Create(t.Context(), super); !errors.Is(err, ErrSuperProjectNotCreatedHere) {
		t.Errorf("got %v, want ErrSuperProjectNotCreatedHere", err)
	}
}

// TestASuperProjectNeverHoldsClinicalData. The constructor drops the request
// rather than refusing it, because clinical data is meaningless for the Project
// that administers the install rather than a contradiction a caller stated.
func TestASuperProjectNeverHoldsClinicalData(t *testing.T) {
	record, err := project.NewSuperProjectRecord(project.Config{
		ID: "prj_super", Slug: "super", Name: "Super Project",
		State: project.StateActive, AllowClinicalData: true,
	})
	if err != nil {
		t.Fatalf("NewSuperProjectRecord: %v", err)
	}

	if record.AllowsClinicalData() {
		t.Error("the super project allows clinical data")
	}

	// The table states the same rule, so neither side can drift.
	_, db := newProjectStore(t)

	_, err = db.ExecContext(t.Context(),
		"INSERT INTO projects (id, kind, slug, name, state, allow_clinical_data,"+
			" created_at, updated_at, state_changed_at)"+
			" VALUES ('prj_x', 'super', 'x', 'X', 'active', 1, 0, 0, 0)")
	if err == nil {
		t.Error("the table accepted a super project holding clinical data")
	}
}

// TestUpdateProjectStateDetectsAVersionConflict, so a decision made against a row
// that has since moved is refused rather than overwriting it.
func TestUpdateProjectStateDetectsAVersionConflict(t *testing.T) {
	store, _ := newProjectStore(t)

	clinic, err := project.NewProject(project.Config{
		ID: "prj_clinic", Slug: "clinic", Name: "Clinic A", State: project.StateActive,
	})
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	version, err := store.Create(t.Context(), clinic)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := store.UpdateState(t.Context(), "prj_clinic", project.StateSuspended, version); err != nil {
		t.Fatalf("first move: %v", err)
	}

	if _, err := store.UpdateState(t.Context(), "prj_clinic", project.StateArchived, version); err == nil {
		t.Error("a stale version overwrote the row")
	}
}

// TestASuspendedProjectIsWhatTheResolverReads ties the lifecycle to what a
// request reaches: the resolver reads the state this store writes.
func TestASuspendedProjectIsWhatTheResolverReads(t *testing.T) {
	store, db := newProjectStore(t)

	clinic, err := project.NewProject(project.Config{
		ID: "prj_clinic", Slug: "clinic", Name: "Clinic A", State: project.StateActive,
	})
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	version, err := store.Create(t.Context(), clinic)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := store.UpdateState(t.Context(), "prj_clinic", project.StateSuspended, version); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	state, err := NewProjectResolver(db).State(t.Context(), "prj_clinic")
	if err != nil {
		t.Fatalf("resolve state: %v", err)
	}

	if state != project.StateSuspended {
		t.Errorf("the resolver reads %q", state)
	}
}
