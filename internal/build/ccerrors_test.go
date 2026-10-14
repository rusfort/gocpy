package build

import "testing"

func TestCCErrors(t *testing.T) {
	in := `/T/gocpy_support.c:877:94: warning: passing 'const gocpy_map *' discards qualifiers [-Wincompatible-pointer-types-discards-qualifiers]
  877 | static void f(gocpy_sb *sb, const void *p) { g(sb, (*(const gocpy_map* *)p)); }
      |                                                                                              ^~~~
/T/gocpy_rt.h:10:6: note: passing argument to parameter 'm' here
   10 | void g(gocpy_sb *sb, gocpy_map *m);
In file included from /T/main.c:1:
/T/main.c:5:3: error: use of undeclared identifier 'x'
    5 |   x = 1;
      |   ^
1 warning and 1 error generated.
Undefined symbols for architecture arm64:
  "_foo", referenced from:
ld: symbol(s) not found for architecture arm64
`
	want := `In file included from /T/main.c:1:
/T/main.c:5:3: error: use of undeclared identifier 'x'
    5 |   x = 1;
      |   ^
Undefined symbols for architecture arm64:
  "_foo", referenced from:
ld: symbol(s) not found for architecture arm64
`
	if got := ccErrors(in); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
