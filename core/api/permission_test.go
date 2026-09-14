package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mirairoad/howl-go/core/api"
)

// An application's permission vocabulary. Two things about it are the whole
// design: each permission is a Go identifier, so an endpoint naming one that
// does not exist does not compile — and this type satisfies api.Permission
// without importing api at all, because Go interfaces are structural. A real
// application declares these in the package it shares with its client, which
// must not be dragging a server framework into a browser build.
type permission string

func (p permission) Permission() string { return string(p) }

const (
	documentsRead  permission = "documents.read"
	documentsWrite permission = "documents.write"
)

// Permissions are the application's business in the same way roles are, and
// this is the whole contract between the two.
func TestPermissionsAreDelegatedToTheApplication(t *testing.T) {
	var asked []string
	route := api.At("POST", "/api/papers", api.Define(api.Spec[api.None, api.None, api.None]{
		Name:        "Papers",
		Permissions: []api.Permission{documentsWrite},
		Handler:     func(r *api.Request[api.None, api.None]) (api.None, error) { return api.None{}, nil },
	}))
	server := serve(t, api.Config{
		Permit: func(r *http.Request, permissions []string) error {
			asked = permissions
			if r.Header.Get("Authorization") == "" {
				return api.Forbidden("not allowed")
			}
			return nil
		},
	}, route)

	res, err := http.Post(server.URL+"/api/papers", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
	// By name, in the order declared: the application looks them up, so it has
	// to get what it wrote down rather than a set in some other order.
	if strings.Join(asked, ",") != "documents.write" {
		t.Fatalf("permissions handed over = %v", asked)
	}

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/papers", nil)
	req.Header.Set("Authorization", "Bearer x")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("permitted status = %d, want 204 for a None response", res.StatusCode)
	}
}

// Declaring permissions without wiring Permit would serve private data to
// everyone, exactly as the roles version would.
func TestPermissionsWithoutPermitPanicAtRegistration(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registration accepted a permission-protected route with no Permit")
		}
	}()
	route := api.At("GET", "/api/papers", api.Define(api.Spec[api.None, api.None, api.None]{
		Name:        "Papers",
		Permissions: []api.Permission{documentsRead},
		Handler:     func(r *api.Request[api.None, api.None]) (api.None, error) { return api.None{}, nil },
	}))
	api.Register(http.NewServeMux(), api.Config{}, route)
}

// An endpoint declaring both requires both. Either alone would make the other
// decorative, and a gate with a decorative half is worse than a gate with one
// half — somebody reading it believes in a check that is not happening.
func TestAnEndpointDeclaringBothRequiresBoth(t *testing.T) {
	route := api.At("GET", "/api/papers", api.Define(api.Spec[api.None, api.None, api.None]{
		Name:        "Papers",
		Roles:       []string{"editor"},
		Permissions: []api.Permission{documentsWrite},
		Handler:     func(r *api.Request[api.None, api.None]) (api.None, error) { return api.None{}, nil },
	}))

	for _, c := range []struct {
		what             string
		role, permission bool
		want             int
	}{
		{"neither", false, false, http.StatusUnauthorized},
		{"the role alone", true, false, http.StatusForbidden},
		{"the permission alone", false, true, http.StatusUnauthorized},
		{"both", true, true, http.StatusNoContent},
	} {
		server := serve(t, api.Config{
			Authorize: func(r *http.Request, roles []string) error {
				if !c.role {
					return api.Unauthorized("no")
				}
				return nil
			},
			Permit: func(r *http.Request, permissions []string) error {
				if !c.permission {
					return api.Forbidden("no")
				}
				return nil
			},
		}, route)
		res, err := http.Get(server.URL + "/api/papers")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Errorf("%s: status = %d, want %d", c.what, res.StatusCode, c.want)
		}
	}
}

// The route table is what the OpenAPI document and any reference screen render
// from, so the names have to survive Define's type erasure.
func TestPermissionNamesSurviveTheRouteTable(t *testing.T) {
	rt := api.Define(api.Spec[api.None, api.None, api.None]{
		Name:        "Papers",
		Permissions: []api.Permission{documentsRead, documentsWrite},
		Handler:     func(r *api.Request[api.None, api.None]) (api.None, error) { return api.None{}, nil },
	})
	if got := strings.Join(api.PermissionNames(rt.Permissions), ","); got != "documents.read,documents.write" {
		t.Errorf("names = %q", got)
	}
	if api.PermissionNames(nil) != nil {
		t.Error("no permissions should be no names, not an empty slice")
	}
}
