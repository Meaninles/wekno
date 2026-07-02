package dbanalytics

import "testing"

func TestMySQLGrantWriteViolationsAllowsReadOnlyGrants(t *testing.T) {
	grants := []string{
		"GRANT USAGE ON *.* TO 'reader'@'%'",
		"GRANT SELECT, SHOW VIEW ON `shop`.* TO 'reader'@'%'",
		"GRANT SELECT (`id`, `name`) ON `shop`.`orders` TO 'reader'@'%'",
	}

	for _, grant := range grants {
		if got := mysqlGrantWriteViolations(grant); len(got) != 0 {
			t.Fatalf("mysqlGrantWriteViolations(%q) = %#v, want none", grant, got)
		}
	}
}

func TestMySQLGrantWriteViolationsRejectsWriteGrants(t *testing.T) {
	grants := []string{
		"GRANT SELECT, INSERT, UPDATE ON `shop`.* TO 'writer'@'%'",
		"GRANT ALL PRIVILEGES ON *.* TO 'root'@'%' WITH GRANT OPTION",
		"GRANT 'app_writer'@'%' TO 'user'@'%'",
	}

	for _, grant := range grants {
		if got := mysqlGrantWriteViolations(grant); len(got) == 0 {
			t.Fatalf("mysqlGrantWriteViolations(%q) returned no violations", grant)
		}
	}
}
