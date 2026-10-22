package directive

import (
	"fmt"
	"testing"
)

func TestZZDbgParse(t *testing.T) {
	f, err := Parse("create host dbg --host 1.2.3.4 --user u --password p --dial-timeout-seconds 7")
	fmt.Println("ERR:", err, "STMTS:", len(f.Statements))
	for _, st := range f.Statements {
		fmt.Println("KIND:", st.Kind, "KEY:", st.Key, "TARGET:", st.Target)
		for name, v := range st.Flags {
			val := "<no value>"
			if v != nil {
				val = *v
			}
			fmt.Printf("FLAG %q = %s\n", name, val)
		}
	}
}
