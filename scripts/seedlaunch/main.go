// Command seedlaunch prepares a database for a SMART conformance run.
//
// A launch needs four things to exist before anybody can test it: a Project, an
// identity that can sign in, a policy that identity holds, and a registration
// naming the address a code comes back to. Creating them through the control
// plane means claiming the install and half a dozen authenticated calls, which
// is a lot of moving parts between a person and the thing they wanted to test.
//
// So this writes them directly, through the same stores the server uses — never
// by hand-written rows, because a row this server could not have written is one
// a run would fail on for reasons that have nothing to do with SMART.
//
// # For a test database, and only for that
//
// It creates an identity whose password is supplied on the command line. Point
// it at a database nobody else uses.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"

	"github.com/Ilavrita/Ilavrita/packages/project"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
	_ "modernc.org/sqlite"
)

func main() {
	var (
		path     = flag.String("db", "", "the database to seed")
		slug     = flag.String("project", "conformance", "the project slug to sign in against")
		email    = flag.String("email", "", "the identity to create")
		password = flag.String("password", "", "its password")
		client   = flag.String("client", "cli_inferno", "the client id to register")
		redirect = flag.String("redirect", "", "the address a code comes back to")
		service  = flag.String("service", "", "also register a backend service under this client id")
	)

	flag.Parse()

	if *path == "" {
		log.Fatal("-db is required")
	}

	// -service alone adds a backend service to a database somebody already
	// seeded, so the two can be run in either order and neither has to know
	// whether the other ran.
	onlyService := *service != "" && *email == ""

	if !onlyService {
		for name, value := range map[string]*string{
			"email": email, "password": password, "redirect": redirect,
		} {
			if *value == "" {
				log.Fatalf("-%s is required", name)
			}
		}

		if err := seed(*path, *slug, *email, *password, *client, *redirect); err != nil {
			log.Fatal(err)
		}

		fmt.Printf("seeded %s: project %s, identity %s, client %s -> %s\n",
			*path, *slug, *email, *client, *redirect)
	}

	if *service != "" {
		private, err := seedService(*path, *slug, *service)
		if err != nil {
			log.Fatal(err)
		}

		// Printed because the suite has to sign with it, and printed alone on a
		// line so a script can take it. It is a key this command just made for a
		// throwaway database; nothing else will ever hold it.
		fmt.Printf("registered backend service %s\nprivate jwks: %s\n", *service, private)
	}
}

// seedService registers a backend service and returns the private key set the
// suite signs its assertions with.
//
// The key is generated here rather than supplied, so a run cannot accidentally
// be given one that is used for anything else.
func seedService(path, slug, client string) (string, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		return "", err
	}

	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}

	public := map[string]string{
		"kty": "RSA", "kid": "conformance", "alg": "RS384", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}

	// The private half, for whoever signs with it. key_ops names what the key is
	// for: a suite that filters on it finds nothing without one, and this server
	// never sees this set at all — it holds only the public half above.
	privateMembers := map[string]any{"key_ops": []string{"sign"}}
	for name, value := range public {
		privateMembers[name] = value
	}

	// RFC 7518 section 6.3.2: a private RSA key carrying p should carry the rest
	// of the Chinese remainder values too, and a suite that rebuilds the key from
	// this set refuses a half-stated one rather than deriving what is missing.
	for name, value := range map[string]*big.Int{
		"d": key.D, "p": key.Primes[0], "q": key.Primes[1],
		"dp": key.Precomputed.Dp, "dq": key.Precomputed.Dq, "qi": key.Precomputed.Qinv,
	} {
		privateMembers[name] = base64.RawURLEncoding.EncodeToString(value.Bytes())
	}

	publicSet, err := json.Marshal(map[string]any{"keys": []map[string]string{public}})
	if err != nil {
		return "", err
	}

	privateSet, err := json.Marshal(map[string]any{"keys": []map[string]any{privateMembers}})
	if err != nil {
		return "", err
	}

	keys, err := project.ParseJWKS(string(publicSet))
	if err != nil {
		return "", err
	}

	ctx := context.Background()

	app, err := project.NewClientApplication(project.ID(slug), project.ClientApplicationConfig{
		// Named after the client id, because a Project holds one name once and a
		// second service seeded into the same database has to be able to exist.
		ID: project.ClientApplicationID(client), Name: "Conformance service " + client,
		State: project.ServiceActive, Kind: project.ClientConfidential, JWKS: keys,
	})
	if err != nil {
		return "", err
	}

	if _, err := sqlite.NewClientApplicationStore(db).Create(ctx, app); err != nil {
		return "", fmt.Errorf("register the service: %w", err)
	}

	member, err := project.NewMembership(project.MembershipConfig{
		ID: project.MembershipID("pm_service_" + client), Project: project.ID(slug), ProjectKind: project.KindStandard,
		Principal: project.PrincipalRef{
			Kind: project.PrincipalClientApplication, ID: project.PrincipalID(client),
		},
		State: project.MembershipActive, Source: project.SourceAPI,
		Policies: []project.PolicyAttachment{{Policy: "pol_conformance"}},
	})
	if err != nil {
		return "", err
	}

	if err := sqlite.NewMembershipStore(db).Create(ctx, member); err != nil {
		return "", fmt.Errorf("give the service standing: %w", err)
	}

	return string(privateSet), nil
}

// seed writes everything one launch needs.
func seed(path, slug, email, password, client, redirect string) error {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		return err
	}

	// One connection, because this driver deadlocks on a query opened inside an
	// open cursor and the stores below read while they write.
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	if err := sqlite.PrepareSchema(ctx, db); err != nil {
		return fmt.Errorf("prepare the schema: %w", err)
	}

	if err := seedProject(ctx, db, slug); err != nil {
		return err
	}

	if err := seedIdentity(ctx, db, slug, email, password); err != nil {
		return err
	}

	return seedClient(ctx, db, slug, client, redirect)
}

// seedProject writes the Project everything else belongs to.
func seedProject(ctx context.Context, db *sql.DB, slug string) error {
	held, err := project.NewProject(project.Config{
		ID: project.ID(slug), Slug: slug, Name: slug, State: project.StateActive,
	})
	if err != nil {
		return err
	}

	if _, err := sqlite.NewProjectStore(db).Create(ctx, held); err != nil {
		return fmt.Errorf("create the project: %w", err)
	}

	return nil
}

// seedIdentity writes an identity that can sign in, the policy it holds, and the
// standing that binds them.
//
// The policy is unrestricted over Organization, which carries no patient data —
// so a conformance run exercises the launch without any of it touching a record
// about a person.
func seedIdentity(ctx context.Context, db *sql.DB, slug, email, password string) error {
	address, err := project.NormaliseEmail(email)
	if err != nil {
		return err
	}

	invited, err := project.NewProjectUser(project.ID(slug), project.UserConfig{
		ID: "usr_conformance", Email: address, State: project.UserInvited,
	})
	if err != nil {
		return err
	}

	users := sqlite.NewUserStore(db)

	version, err := users.Create(ctx, invited)
	if err != nil {
		return fmt.Errorf("create the identity: %w", err)
	}

	hash, err := project.HashPassword(password, rand.Reader)
	if err != nil {
		return err
	}

	if _, err := users.AcceptInvitation(ctx, "usr_conformance", hash, version); err != nil {
		return fmt.Errorf("set the password: %w", err)
	}

	for _, statement := range []string{
		"INSERT INTO access_policies (project_id, id, name, created_at, updated_at)" +
			" VALUES (?, 'pol_conformance', 'Conformance', 0, 0)",
		"INSERT INTO access_policy_rules (project_id, policy_id, ordinal, kind, res_type, action," +
			" unrestricted, compartment_type, compartment_id, compartment_param) VALUES" +
			" (?, 'pol_conformance', 0, 'fhir', 'Organization', 'read', 1, NULL, NULL, NULL)," +
			" (?, 'pol_conformance', 1, 'fhir', 'Organization', 'search', 1, NULL, NULL, NULL)," +
			" (?, 'pol_conformance', 2, 'fhir', 'Organization', 'write', 1, NULL, NULL, NULL)",
	} {
		arguments := make([]any, 0, 4)
		for range countPlaceholders(statement) {
			arguments = append(arguments, slug)
		}

		if _, err := db.ExecContext(ctx, statement, arguments...); err != nil {
			return fmt.Errorf("write the policy: %w", err)
		}
	}

	member, err := project.NewMembership(project.MembershipConfig{
		ID: "pm_conformance", Project: project.ID(slug), ProjectKind: project.KindStandard,
		Principal: project.PrincipalRef{Kind: project.PrincipalUser, ID: "usr_conformance"},
		State:     project.MembershipActive, Source: project.SourceInvite,
		Policies: []project.PolicyAttachment{{Policy: "pol_conformance"}},
	})
	if err != nil {
		return err
	}

	if err := sqlite.NewMembershipStore(db).Create(ctx, member); err != nil {
		return fmt.Errorf("give the identity standing: %w", err)
	}

	return nil
}

// seedClient registers the app a run authorizes as.
func seedClient(ctx context.Context, db *sql.DB, slug, client, redirect string) error {
	addresses, err := project.NewRedirectURIs(redirect)
	if err != nil {
		return err
	}

	app, err := project.NewClientApplication(project.ID(slug), project.ClientApplicationConfig{
		ID: project.ClientApplicationID(client), Name: "Conformance app",
		State: project.ServiceActive, Kind: project.ClientPublic, RedirectURIs: addresses,
	})
	if err != nil {
		return err
	}

	if _, err := sqlite.NewClientApplicationStore(db).Create(ctx, app); err != nil {
		return fmt.Errorf("register the app: %w", err)
	}

	return nil
}

// countPlaceholders says how many values one statement binds.
func countPlaceholders(statement string) []struct{} {
	held := 0

	for _, one := range statement {
		if one == '?' {
			held++
		}
	}

	return make([]struct{}, held)
}
