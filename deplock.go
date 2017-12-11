package main

import (
	"fmt"
	"io/ioutil"

	"github.com/pelletier/go-toml"
)

type rawLock struct {
	Projects []rawLockedProject `toml:"projects"`
}

type rawLockedProject struct {
	Name     string   `toml:"name"`
	Branch   string   `toml:"branch,omitempty"`
	Revision string   `toml:"revision"`
	Version  string   `toml:"version,omitempty"`
	Source   string   `toml:"source,omitempty"`
	Packages []string `toml:"packages"`
}

func parseTOMLLockFile(file string) ([]*depInfo, error) {
	data, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}
	raw := rawLock{}
	err = toml.Unmarshal(data, &raw)
	if err != nil {
		return nil, err
	}
	var deps []*depInfo
	for _, proj := range raw.Projects {
		if proj.Source != "" {
			return nil, fmt.Errorf("project %q has external source; godeps does not work with external sources", proj.Name)
		}
		deps = append(deps, &depInfo{
			project: proj.Name,
			VCSInfo: VCSInfo{
				revid: proj.Revision,
			},
		})
	}
	return deps, nil
}
