package project

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestAMintedIdCarriesItsFamilyPrefix keeps the three principal namespaces
// disjoint. ux_pm_active_principal is unique on the generated principal_id
// alone, so two families sharing an id would be one principal to it.
func TestAMintedIdCarriesItsFamilyPrefix(t *testing.T) {
	application, err := MintClientApplicationID(entropy('a'))
	if err != nil {
		t.Fatalf("MintClientApplicationID: %v", err)
	}

	bot, err := MintBotID(entropy('b'))
	if err != nil {
		t.Fatalf("MintBotID: %v", err)
	}

	credential, err := MintCredentialID(entropy('c'))
	if err != nil {
		t.Fatalf("MintCredentialID: %v", err)
	}

	minted := map[string]string{
		string(application): clientApplicationPrefix,
		string(bot):         botPrefix,
		string(credential):  credentialPrefix,
	}

	for id, prefix := range minted {
		if !strings.HasPrefix(id, prefix) {
			t.Errorf("%q does not begin %q", id, prefix)
		}

		if len(id) <= len(prefix) {
			t.Errorf("%q is its prefix and nothing else", id)
		}
	}
}

// TestAnIdIsMintedWholeOrNotAtAll, so a short read yields no identifier rather
// than a predictable one.
func TestAnIdIsMintedWholeOrNotAtAll(t *testing.T) {
	short := strings.NewReader("too short")

	if id, err := MintClientApplicationID(short); err == nil || id != "" {
		t.Errorf("a short read minted %q with error %v", id, err)
	}
}

// TestAnIdOutsideItsNamespaceIsRefused mirrors the table's own substr CHECK,
// which is deliberately not LIKE: SQLite's LIKE is ASCII-case-insensitive and
// accepts 'CLI_x'.
func TestAnIdOutsideItsNamespaceIsRefused(t *testing.T) {
	refused := []ClientApplicationID{"", "cli_", "CLI_loader", "loader", "bot_loader", "usr_loader"}

	for _, id := range refused {
		if err := ValidateClientApplicationID(id); !errors.Is(err, ErrInvalidServiceID) {
			t.Errorf("ValidateClientApplicationID(%q): got %v, want ErrInvalidServiceID", id, err)
		}
	}

	if err := ValidateClientApplicationID("cli_loader"); err != nil {
		t.Errorf("a well-formed id was refused: %v", err)
	}

	if err := ValidateBotID("cli_loader"); !errors.Is(err, ErrInvalidServiceID) {
		t.Errorf("a client application id passed as a bot id: %v", err)
	}
}

// TestNewClientApplicationRefusesAnIncompleteRegistration, because nothing an
// operator must revoke in a hurry is nameless or stateless.
func TestNewClientApplicationRefusesAnIncompleteRegistration(t *testing.T) {
	complete := ClientApplicationConfig{
		ID: "cli_loader", Name: "Nightly loader",
		State: ServiceActive, Kind: ClientConfidential,
	}

	cases := map[string]struct {
		owner  ID
		mutate func(ClientApplicationConfig) ClientApplicationConfig
		want   error
	}{
		"no owning project": {
			"", func(c ClientApplicationConfig) ClientApplicationConfig { return c }, ErrInvalidProjectID,
		},
		"the wildcard project": {
			"*", func(c ClientApplicationConfig) ClientApplicationConfig { return c }, ErrInvalidProjectID,
		},
		"no identifier": {"prj_a", func(c ClientApplicationConfig) ClientApplicationConfig {
			c.ID = ""

			return c
		}, ErrInvalidServiceID},
		"no name": {"prj_a", func(c ClientApplicationConfig) ClientApplicationConfig {
			c.Name = ""

			return c
		}, ErrMissingServiceName},
		"a state outside the enum": {"prj_a", func(c ClientApplicationConfig) ClientApplicationConfig {
			c.State = "retired"

			return c
		}, ErrUnknownState},
	}

	for name, testCase := range cases {
		_, err := NewClientApplication(testCase.owner, testCase.mutate(complete))
		if !errors.Is(err, testCase.want) {
			t.Errorf("%s: got %v, want %v", name, err, testCase.want)
		}
	}
}

// TestAClientApplicationConfigNamesNoProjectOfItsOwn keeps the owning Project a
// constructor argument, so a caller cannot pair a registration with a Project it
// does not belong to.
func TestAClientApplicationConfigNamesNoProjectOfItsOwn(t *testing.T) {
	configs := map[string]reflect.Type{
		"ClientApplicationConfig": reflect.TypeOf(ClientApplicationConfig{}),
		"BotConfig":               reflect.TypeOf(BotConfig{}),
		"CredentialConfig":        reflect.TypeOf(CredentialConfig{}),
	}

	for name, config := range configs {
		for index := range config.NumField() {
			if field := config.Field(index); field.Type == reflect.TypeOf(ID("")) {
				t.Errorf("%s.%s carries a Project, so one can be paired wrongly", name, field.Name)
			}
		}
	}
}

// TestABotHasNoSecretFieldAtAll. A bot is invoked by this server rather than
// authenticated by it, so a bot secret is unrepresentable, not merely unissued.
func TestABotHasNoSecretFieldAtAll(t *testing.T) {
	forbidden := map[reflect.Type]string{
		reflect.TypeOf(ClientSecret{}):     "ClientSecret",
		reflect.TypeOf(CredentialHash("")): "CredentialHash",
	}

	bot := reflect.TypeOf(Bot{})
	for index := range bot.NumField() {
		field := bot.Field(index)
		if carried, holds := forbidden[field.Type]; holds {
			t.Errorf("Bot.%s carries a %s, so a bot secret has become expressible", field.Name, carried)
		}
	}
}

// TestRevokedIsTerminalForARegistration. A returning integration is a new
// registration, not a revived one: reviving would resurrect every membership and
// policy binding that ever named it.
func TestRevokedIsTerminalForARegistration(t *testing.T) {
	every := []ServiceState{ServiceActive, ServiceSuspended, ServiceRevoked}

	for _, next := range every {
		if ServiceRevoked.CanTransitionTo(next) {
			t.Errorf("revoked moved to %s", next)
		}
	}

	if !ServiceActive.CanTransitionTo(ServiceSuspended) || !ServiceSuspended.CanTransitionTo(ServiceActive) {
		t.Error("a suspension is not reversible, so it is a revocation by another name")
	}
}

// TestAServiceTransitionFromAnUnknownStateFailsClosed, rather than reading an
// unrecognised state as the nearest known one.
func TestAServiceTransitionFromAnUnknownStateFailsClosed(t *testing.T) {
	if _, err := ServiceState("retired").TransitionTo(ServiceActive); !errors.Is(err, ErrUnknownState) {
		t.Errorf("got %v, want ErrUnknownState", err)
	}

	if _, err := ServiceActive.TransitionTo("retired"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("got %v, want ErrInvalidTransition", err)
	}
}

// TestARegistrationNamesItsOwnPrincipalKind, so no caller can pair an id with
// another family's kind.
func TestARegistrationNamesItsOwnPrincipalKind(t *testing.T) {
	app := activeApplication(t)
	if got := app.Principal(); got.Kind != PrincipalClientApplication || got.ID != "cli_loader" {
		t.Errorf("client application principal is %v", got)
	}

	bot, err := NewBot("prj_a", BotConfig{ID: "bot_worker", Name: "Nightly worker", State: ServiceActive})
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	if got := bot.Principal(); got.Kind != PrincipalBot || got.ID != "bot_worker" {
		t.Errorf("bot principal is %v", got)
	}
}

// machineMembership is the config each privilege guard is checked against, so a
// case states only the one field it is about.
func machineMembership(kind PrincipalKind, id PrincipalID) MembershipConfig {
	return MembershipConfig{
		ID: "mem_1", Project: "prj_a", ProjectKind: KindStandard,
		Principal: PrincipalRef{Kind: kind, ID: id},
		State:     MembershipActive, Source: SourceAPI,
	}
}

// TestAMachinePrincipalIsRefusedSuperAdmin. Administering the install is
// answerable work, and a secret answers to no one.
func TestAMachinePrincipalIsRefusedSuperAdmin(t *testing.T) {
	machines := map[PrincipalKind]PrincipalID{
		PrincipalClientApplication: "cli_loader",
		PrincipalBot:               "bot_worker",
	}

	for kind, id := range machines {
		cfg := machineMembership(kind, id)
		cfg.ProjectKind, cfg.Admin, cfg.SuperAdmin = KindSuper, true, true

		if _, err := NewMembership(cfg); !errors.Is(err, ErrMachinePrincipalPrivilege) {
			t.Errorf("%s held super admin: got %v", kind, err)
		}
	}

	human := machineMembership(PrincipalUser, "usr_1")
	human.ProjectKind, human.Admin, human.SuperAdmin = KindSuper, true, true

	if _, err := NewMembership(human); err != nil {
		t.Errorf("a user was refused super admin in the super project: %v", err)
	}
}

// TestABotHoldsNoAdministrativeStanding. A bot runs code this server invokes, so
// admin on one is a control-plane write reachable from whatever that code does.
func TestABotHoldsNoAdministrativeStanding(t *testing.T) {
	cfg := machineMembership(PrincipalBot, "bot_worker")
	cfg.Admin = true

	if _, err := NewMembership(cfg); !errors.Is(err, ErrBotPrivilege) {
		t.Errorf("a bot held admin: got %v", err)
	}

	application := machineMembership(PrincipalClientApplication, "cli_loader")
	application.Admin = true

	if _, err := NewMembership(application); err != nil {
		t.Errorf("a client application was refused project admin: %v", err)
	}
}

// TestAMachinePrincipalCarriesNoProfile. Its authority is its AccessPolicy,
// never a compartment it occupies: a profile would collect compartment grants
// with no person in the chain that leads to them.
func TestAMachinePrincipalCarriesNoProfile(t *testing.T) {
	profile := &ProfileRef{Type: "Practitioner", ID: "prac_1"}

	for _, kind := range []PrincipalKind{PrincipalClientApplication, PrincipalBot} {
		cfg := machineMembership(kind, PrincipalID("x"))
		cfg.Profile = profile

		if _, err := NewMembership(cfg); !errors.Is(err, ErrMachinePrincipalProfile) {
			t.Errorf("%s carried a profile: got %v", kind, err)
		}
	}

	human := machineMembership(PrincipalUser, "usr_1")
	human.Profile = profile

	if _, err := NewMembership(human); err != nil {
		t.Errorf("a user was refused a profile: %v", err)
	}
}
