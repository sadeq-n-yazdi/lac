# Making `go install sadeq.uk/lac/...` work

The module is called `sadeq.uk/lac`, but the code lives on GitHub. For the Go toolchain to connect
the two, `sadeq.uk` has to serve one small page saying where the repository is.

`https://sadeq.uk/lac?go-get=1` already serves such a page. **It names the wrong module**, so
`go install` still fails:

```html
<!-- what is served today -->
<meta name="go-import" content="code.sadeq.uk/lac git https://github.com/sadeq-n-yazdi/lac">
```

The first field has to match the module path exactly. The toolchain asks about `sadeq.uk/lac`, is
told about `code.sadeq.uk/lac`, and refuses the mismatch.

Building from a checkout works regardless; this is only about installing by module path.

## The one-word fix

On whichever repository backs `sadeq.uk`, in the page served at `/lac`, drop the `code.` prefix:

```html
<meta name="go-import" content="sadeq.uk/lac git https://github.com/sadeq-n-yazdi/lac">
```

While you are there, `go-source` makes pkg.go.dev link to the right files:

```html
<meta name="go-source" content="sadeq.uk/lac
  https://github.com/sadeq-n-yazdi/lac
  https://github.com/sadeq-n-yazdi/lac/tree/main{/dir}
  https://github.com/sadeq-n-yazdi/lac/blob/main{/dir}/{file}#L{line}">
```

Nothing is needed for the subdirectories. The toolchain asks for `sadeq.uk/lac/cmd/lac?go-get=1`
first, and when that is not found it walks up to `sadeq.uk/lac?go-get=1`, which is this page.

If the site is built by Jekyll, either add empty front matter (`---` twice at the top of the file)
or put a `.nojekyll` file at the repository root, so the HTML is served as written.

## Checking it worked

```sh
curl "https://sadeq.uk/lac?go-get=1" | grep go-import
go install sadeq.uk/lac/cmd/lac@latest
go install sadeq.uk/lac/cmd/lacd@latest
```

The first should print a meta tag naming `sadeq.uk/lac`; the other two should put `lac` and `lacd`
in `$(go env GOPATH)/bin`.

The module proxy caches what it fetches, so if you tried an install while the old page was up, give
it a few minutes or test with `GOPROXY=direct` to go straight to the source.

## If you would rather not

Change the module path in `go.mod` to `github.com/sadeq-n-yazdi/lac` and update the imports; the
toolchain then resolves it with no page to publish at all.
