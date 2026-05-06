package project

var (
	description = "The tunnelport does something."
	gitSHA      = "n/a"
	name        = "tunnelport"
	source      = "https://github.com/giantswarm/tunnelport"
	version     = "0.1.0-dev"
)

func Description() string {
	return description
}

func GitSHA() string {
	return gitSHA
}

func Name() string {
	return name
}

func Source() string {
	return source
}

func Version() string {
	return version
}
