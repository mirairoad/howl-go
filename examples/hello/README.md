# howl-go hello

A standalone Go module consuming howl-go from GitHub. It intentionally has no
`replace` directive, so its build verifies the public module works for external
applications.

```bash
make
./hello
```

Open <http://localhost:9000>.
