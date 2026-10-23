package neo4j

import (
	"context"
	"errors"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j/db"

	"github.com/xavidop/tobira/driver"
)

func TestKindForCode(t *testing.T) {
	for code, want := range map[string]error{
		"Neo.ClientError.Security.Unauthorized":                     driver.ErrAuth,
		"Neo.ClientError.Security.Forbidden":                        driver.ErrPermission,
		"Neo.ClientError.Statement.AccessMode":                      driver.ErrReadOnly,
		"Neo.ClientError.Transaction.ForbiddenDueToTransactionType": driver.ErrReadOnly,
		"Neo.ClientError.Database.DatabaseNotFound":                 driver.ErrNotFound,
		"Neo.ClientError.Schema.ConstraintValidationFailed":         driver.ErrConstraint,
		"Neo.ClientError.Statement.SyntaxError":                     driver.ErrSyntax,
		"Neo.ClientError.Statement.ParameterMissing":                driver.ErrSyntax,
		"Neo.TransientError.Transaction.DeadlockDetected":           driver.ErrConflict,
		"Neo.DatabaseError.General.UnknownError":                    nil,
	} {
		if got := kindForCode(code, ""); got != want {
			t.Errorf("%s: %v, want %v", code, got, want)
		}
	}
}

func TestMapError(t *testing.T) {
	err := mapError(context.Background(), "query", &db.Neo4jError{Code: "Neo.ClientError.Statement.SyntaxError", Msg: "Invalid input"})
	var de *driver.Error
	if !errors.As(err, &de) || de.Kind != driver.ErrSyntax || de.Driver != Name || de.Code != "Neo.ClientError.Statement.SyntaxError" {
		t.Fatalf("mapError = %#v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = mapError(ctx, "query", errors.New("connection reset"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("an error under a cancelled context does not match context.Canceled: %v", err)
	}
	if mapError(ctx, "query", nil) != nil {
		t.Fatal("nil was wrapped")
	}
	wrapped := driver.Wrap(driver.ErrNotFound, Name, "x", errors.New("x"))
	if mapError(context.Background(), "query", wrapped) != wrapped {
		t.Fatal("a *driver.Error was re-wrapped")
	}
}
