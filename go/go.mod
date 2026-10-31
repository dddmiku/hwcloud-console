module hwcloud

go 1.23.1

require (
	github.com/mattn/go-ieproxy v0.0.12
	github.com/playwright-community/playwright-go v0.6100.0
)

require (
	github.com/deckarep/golang-set/v2 v2.8.0 // indirect
	github.com/go-jose/go-jose/v3 v3.0.5 // indirect
	github.com/go-stack/stack v1.8.1 // indirect
	golang.org/x/net v0.25.0 // indirect
	golang.org/x/sys v0.20.0 // indirect
	golang.org/x/text v0.15.0 // indirect
)

replace github.com/playwright-community/playwright-go => ./third_party/playwright-go
