# brokelabs-cli

The `brokelabs` command line of BrokeLabs:

- **Operators:** install and manage the platform on a Linux machine or cluster (`brokelabs install`, from M2).
- **Challenge authors:** write, test and publish challenges (`brokelabs challenge …`).

> Status: M1. Available today: `brokelabs version`, `brokelabs challenge lint`, `brokelabs challenge manifest`, `brokelabs challenge images`.

## Usage

```sh
brokelabs challenge lint path/to/challenge   # same validation as the operator at install time
brokelabs challenge manifest path/to/challenge | kubectl apply -f -   # install a local challenge (development, unsigned)
brokelabs challenge images path/to/challenge     # machine images, and the ones built from images/<machine>/
```

## Development

Requirements: Go (version in `go.mod`), make.

```sh
make lint test build   # bin/brokelabs (linux/amd64)
make install           # into $GOBIN
```

## License

[MIT](LICENSE)
