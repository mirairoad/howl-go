package db_test

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mirairoad/howl-go/db"
	"github.com/mirairoad/howl-go/db/memdb"
)

// User is a collection: an ordinary struct with the envelope embedded. The
// JSON tags are the stored field names — there is no second schema.
type User struct {
	db.Doc
	Email string   `json:"email"`
	Name  string   `json:"name"`
	Plan  string   `json:"plan"`
	Seats int64    `json:"seats"`
	Tags  []string `json:"tags"`
}

// Defaults runs on create, before validation.
func (u *User) Defaults() {
	if u.Plan == "" {
		u.Plan = "free"
	}
	if u.Tags == nil {
		u.Tags = []string{}
	}
}

// Validate runs on create and on every patch. A returned error becomes
// db.ErrInvalid, which an endpoint maps to a 400.
func (u *User) Validate() error {
	if !strings.Contains(u.Email, "@") {
		return errors.New("email: not an address")
	}
	if u.Name == "" {
		return errors.New("name: required")
	}
	return nil
}

func Example() {
	ctx := context.Background()

	// In an application this is pg.New[User](ctx, conn, pg.Options{…}); the
	// service it returns is the same one either way.
	users, err := memdb.NewService[User](db.Options{Collection: "users"})
	if err != nil {
		panic(err)
	}

	ada, err := users.Create(ctx, User{Email: "ada@example.com", Name: "Ada"}, db.By("signup"))
	if err != nil {
		panic(err)
	}
	fmt.Println("created:", ada.Name, ada.Plan, "v"+fmt.Sprint(ada.Version))

	// A patch is a closure over the document the service just read. Field
	// names are checked by the compiler, and the write is version-locked.
	ada, err = users.Patch(ctx, ada.ID, func(u *User) {
		u.Name = "Ada Lovelace"
		u.Seats = 3
	}, db.By("u_admin"))
	if err != nil {
		panic(err)
	}
	fmt.Println("patched:", ada.Name, ada.Seats, "v"+fmt.Sprint(ada.Version), "by", ada.Meta.UpdatedBy)

	if _, err := users.Create(ctx, User{Email: "not-an-address", Name: "Nope"}); errors.Is(err, db.ErrInvalid) {
		fmt.Println("rejected:", err)
	}

	// Soft delete is the default: the row stays, and every read skips it.
	if err := users.Delete(ctx, ada.ID, db.By("u_admin")); err != nil {
		panic(err)
	}
	_, err = users.Get(ctx, ada.ID)
	fmt.Println("after delete:", errors.Is(err, db.ErrNotFound))

	restored, err := users.Restore(ctx, ada.ID)
	if err != nil {
		panic(err)
	}
	fmt.Println("restored:", restored.Name)

	// Output:
	// created: Ada free v1
	// patched: Ada Lovelace 3 v2 by u_admin
	// rejected: db: invalid document: users: email: not an address
	// after delete: true
	// restored: Ada Lovelace
}

func ExampleService_Find() {
	ctx := context.Background()
	users, _ := memdb.NewService[User](db.Options{Collection: "users"})

	for _, u := range []User{
		{Email: "a@example.com", Name: "Ada", Plan: "pro", Seats: 9},
		{Email: "g@example.com", Name: "Grace", Plan: "pro", Seats: 2},
		{Email: "k@example.com", Name: "Katherine", Plan: "free", Seats: 1},
	} {
		if _, err := users.Create(ctx, u); err != nil {
			panic(err)
		}
	}

	// Typed constructors, or the same filter written as a literal map — both
	// compile to the same predicate.
	pro, err := users.Find(ctx, db.Query{
		Where: db.And(db.Eq("plan", "pro"), db.Gte("seats", 2)),
		Sort:  db.Desc("seats"),
	})
	if err != nil {
		panic(err)
	}
	for _, u := range pro {
		fmt.Println(u.Name, u.Seats)
	}

	n, err := users.Count(ctx, db.Query{Where: db.M{"plan": "free"}})
	if err != nil {
		panic(err)
	}
	fmt.Println("free:", n)

	// A domain lookup is One with a filter — the service has no getByEmail,
	// because your package does.
	grace, err := users.One(ctx, db.Query{Where: db.Eq("email", "g@example.com")})
	if err != nil {
		panic(err)
	}
	fmt.Println("found:", grace.Name)

	// Output:
	// Ada 9
	// Grace 2
	// free: 1
	// found: Grace
}

// A field added to the struct is not a migration — but the documents written
// before it exists do not carry it, and Report is how you find out.
func ExampleService_Report() {
	ctx := context.Background()
	users, _ := memdb.NewService[User](db.Options{Collection: "users"})
	ada, _ := users.Create(ctx, User{Email: "a@example.com", Name: "Ada"})

	// Stand in for a document written by an older version of the struct: no
	// `plan` key, and a `nickname` key nothing declares any more.
	if _, err := users.Backend().UpdatePaths(ctx, ada.ID,
		map[string]any{"nickname": "the countess"},
		db.UpdateOptions{Unset: []string{"plan"}, NoBump: true}); err != nil {
		panic(err)
	}

	report, err := users.Report(ctx)
	if err != nil {
		panic(err)
	}
	for _, field := range report.Missing {
		fmt.Printf("missing %s in %d/%d documents, backfill with %s\n",
			field.Field, field.Docs, report.Total, field.Default)
	}
	for _, field := range report.Orphans {
		fmt.Printf("orphan %s in %d documents\n", field.Field, field.Docs)
	}

	if _, err := users.Backfill(ctx, "plan"); err != nil {
		panic(err)
	}
	if _, err := users.DropField(ctx, "nickname"); err != nil {
		panic(err)
	}

	report, _ = users.Report(ctx)
	fmt.Println("clean:", len(report.Missing) == 0 && len(report.Orphans) == 0)

	// Output:
	// missing plan in 1/1 documents, backfill with "free"
	// orphan nickname in 1 documents
	// clean: true
}
