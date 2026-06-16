package scaffold

import _ "embed"

// ExampleSpec is a fully-annotated starter pilot.app.yaml, emitted by
// `pilot-app example`. Copy it, edit it, then `pilot-app init`.
//
//go:embed templates/example.pilot.app.yaml
var ExampleSpec string
