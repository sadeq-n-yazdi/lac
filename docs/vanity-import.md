# Making `go install code.sadeq.uk/lac/...` work

The module is called `code.sadeq.uk/lac`, but the code lives on GitHub. For the Go toolchain to
connect the two, `code.sadeq.uk` has to serve one small page saying where the repository is. Until
it does, `go install code.sadeq.uk/lac/cmd/lac@latest` fails with:

```
unrecognized import path "code.sadeq.uk/lac/cmd/lac": reading
https://code.sadeq.uk/lac/cmd/lac?go-get=1: 404 Not Found
```

Building from a checkout works regardless; this is only about installing by module path.

## What to publish

`code.sadeq.uk` is served by GitHub Pages. Add one file to whichever repository backs it, at
**`lac/index.html`**:

```html
<!DOCTYPE html>
<html>
  <head>
    <meta charset="utf-8">
    <meta name="go-import" content="code.sadeq.uk/lac git https://github.com/sadeq-n-yazdi/lac">
    <meta name="go-source" content="code.sadeq.uk/lac
      https://github.com/sadeq-n-yazdi/lac
      https://github.com/sadeq-n-yazdi/lac/tree/main{/dir}
      https://github.com/sadeq-n-yazdi/lac/blob/main{/dir}/{file}#L{line}">
    <meta http-equiv="refresh" content="0; url=https://github.com/sadeq-n-yazdi/lac">
  </head>
  <body>
    Redirecting to <a href="https://github.com/sadeq-n-yazdi/lac">github.com/sadeq-n-yazdi/lac</a>.
  </body>
</html>
```

The `go-import` line is the part that matters: it tells the toolchain the module rooted at
`code.sadeq.uk/lac` is a git repository at that GitHub URL. `go-source` makes pkg.go.dev link to
the right files, and the refresh sends a person who follows the link somewhere useful.

Nothing is needed for the subdirectories. The toolchain asks for `code.sadeq.uk/lac/cmd/lac?go-get=1`
first, and when that is not found it walks up to `code.sadeq.uk/lac?go-get=1`, which is this file.

If the Pages site is built by Jekyll, either add empty front matter (`---` twice at the top of the
file) or put a `.nojekyll` file at the repository root, so the HTML is served as written.

## Checking it worked

```sh
curl "https://code.sadeq.uk/lac?go-get=1" | grep go-import
go install code.sadeq.uk/lac/cmd/lac@latest
go install code.sadeq.uk/lac/cmd/lacd@latest
```

The first should print the meta tag; the other two should put `lac` and `lacd` in
`$(go env GOPATH)/bin`.

## If you would rather not

Change the module path in `go.mod` to `github.com/sadeq-n-yazdi/lac` and update the imports; the
toolchain then resolves it with no page to publish. It is a one-line change in `go.mod` and a
find-and-replace across the imports, but it does change the module's identity, so it is better done
before anyone depends on it than after.
