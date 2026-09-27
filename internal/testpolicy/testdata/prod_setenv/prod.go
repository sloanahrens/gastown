package prodsetenv

import "os"

func Configure() {
	os.Setenv("A", "b")
}
