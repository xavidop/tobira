package drivertest

import (
	"fmt"
	"slices"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Dataset is a typed fixture: column definitions plus rows of driver.Values.
type Dataset struct {
	Columns []driver.Column
	Rows    []driver.Row
	Key     []string
}

// People is the standard dataset: 57 rows (so 3 full pages of 20 plus a
// remainder) with id (int PK), name, email (unique), age (nullable int),
// score (decimal), active (bool), created_at (UTC time) and tags (JSON array).
//
// Callers must not mutate People; use PeopleDataset for a private copy.
var People = buildPeople()

const peopleCollection = "tobira_people"

// PeopleCollection returns the name of the collection holding People.
func PeopleCollection() string { return peopleCollection }

// PeopleDataset returns a deep copy of People that callers may modify.
func PeopleDataset() Dataset {
	d := Dataset{
		Columns: slices.Clone(People.Columns),
		Rows:    make([]driver.Row, len(People.Rows)),
		Key:     slices.Clone(People.Key),
	}
	for i, r := range People.Rows {
		d.Rows[i] = slices.Clone(r) // Values are immutable
	}
	return d
}

var peopleNames = []string{
	"Ōtani Shōhei", "Conan O'Brien", "Ada Lovelace", "Grace Hopper", "Alan Turing",
	"Linus Torvalds", "Margaret Hamilton", "Dennis Ritchie", "Barbara Liskov", "Ken Thompson",
	"José Álvarez", "Zoë Müller", "李小龍", "Søren Kierkegaard", "Anita Borg",
}

var peopleTags = []string{
	`["admin","beta"]`, `["beta"]`, `[]`, `["admin"]`, `["staff","beta","eu"]`, `[]`,
}

func buildPeople() Dataset {
	cols := []driver.Column{
		{Name: "id", Type: driver.TypeInt, Key: true},
		{Name: "name", Type: driver.TypeString},
		{Name: "email", Type: driver.TypeString},
		{Name: "age", Type: driver.TypeInt},
		{Name: "score", Type: driver.TypeDecimal},
		{Name: "active", Type: driver.TypeBool},
		{Name: "created_at", Type: driver.TypeTime},
		{Name: "tags", Type: driver.TypeJSON},
	}
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rows := make([]driver.Row, 57)
	for i := range rows {
		id := i + 1
		age := driver.Int(int64(20 + id%45))
		if id%11 == 0 {
			age = driver.Null() // 11, 22, 33, 44, 55
		}
		score := fmt.Sprintf("%d.%02d", (id*37)%500, (id*13)%100)
		if id == 1 {
			score = "12.50"
		}
		rows[i] = driver.Row{
			driver.Int(int64(id)),
			driver.String(peopleNames[i%len(peopleNames)]),
			driver.String(fmt.Sprintf("user%02d@example.com", id)),
			age,
			driver.Decimal(score),
			driver.Bool(id%3 != 0),
			driver.Time(base.Add(time.Duration(i) * 26 * time.Hour)),
			driver.JSON([]byte(peopleTags[i%len(peopleTags)])),
		}
	}
	return Dataset{Columns: cols, Rows: rows, Key: []string{"id"}}
}
