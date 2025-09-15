# go-domaindb

Self-contained domain database library for Go.

**See also: [go-ipdb](https://github.com/termermc/go-ipdb)**

Automatically downloads and caches domain databases for local in-process querying without reaching out to external APIs.

Supports the following operations:
 - Single-source domain lists
 - Composite domain lists
 - Optional bring-your-own download logic
 - Optional bring-your-own caching logic

Requests for domain data do not leave your process, and you can aggregate multiple data sources into a single database for better accuracy.

Even if remote lists go down, the library can still function by using cached data.
If you specify multiple data sources, the failing sources will be skipped and the remaining sources will be used.

## Use Cases

This library is useful if:
- You need to prevent spam from disposable email addresses
- You need to enforce Fediverse instance block lists
- You don't want to rely on an external service for email data
- You have strict privacy requirements that prevent using an external service for domain data
- You need to aggregate multiple domain data sources into a single database

## Download

Add to your project by running:

```bash
go get github.com/termermc/go-domaindb
```

## Examples

See [examples](examples) for more usage examples.

Below are some simpler examples demonstrating the basic usage of the library.

## Disposable Check

If you have initialized a `DomainDb` with a database named "disposable", you can check if a domain exists inside of it:

```go
domain := "10minutesmail.com"

isDisposable, err := domainDb.DoesDbHaveDomain("disposable", domain)
if err != nil {
	panic(err)
}

if isDisposable {
	println("Domain is a disposable email address")
}
```

## Obtaining Database Files

You can find many different domain lists for different purposes online. The only requirement is that lists are newline-separated and contain a domain per line.
Blank lines and lines starting with `#` are also ignored.
A few list URLs are included in the examples directory.
Googling will yield more results. You should avoid any lists that are not updated frequently.

Please keep in mind that the more lists you use, the more memory your process will consume.
The in-memory representation of a database may be larger than the database file itself.
