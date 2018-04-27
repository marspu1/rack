package local

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/convox/rack/helpers"
	"github.com/convox/rack/manifest"
	"github.com/convox/rack/options"
	"github.com/convox/rack/structs"
	"github.com/pkg/errors"
)

var convergeLock sync.Mutex

func (p *Provider) converge(app string) error {
	convergeLock.Lock()
	defer convergeLock.Unlock()

	log := p.logger("converge").Append("app=%q", app)

	m, r, err := helpers.AppManifest(p, app)
	if err != nil {
		return log.Error(err)
	}

	desired := []container{}

	a, err := p.AppGet(app)
	if err != nil {
		return err
	}

	if !a.Sleep {
		var c []container

		c, err = p.resourceContainers(m.Resources, app, r.Id)
		if err != nil {
			return errors.WithStack(log.Error(err))
		}

		desired = append(desired, c...)

		c, err = p.serviceContainers(m.Services, app, r.Id)
		if err != nil {
			return errors.WithStack(log.Error(err))
		}

		desired = append(desired, c...)
	}

	current, err := containersByLabels(map[string]string{
		"convox.rack": p.Name,
		"convox.app":  app,
	})
	if err != nil {
		return errors.WithStack(log.Error(err))
	}

	extra := diffContainers(current, desired)
	needed := diffContainers(desired, current)

	for _, c := range extra {
		if err := p.containerStop(c.Id); err != nil {
			return errors.WithStack(log.Error(err))
		}
	}

	for _, c := range needed {
		if _, err := p.containerStart(c, app, r.Id); err != nil {
			return errors.WithStack(log.Error(err))
		}
	}

	if err := p.route(app); err != nil {
		return errors.WithStack(log.Error(err))
	}

	// if err := p.routeContainers(desired); err != nil {
	//   return errors.WithStack(log.Error(err))
	// }

	return log.Success()
}

func (p *Provider) idle() error {
	log := p.logger("idle")

	r, err := p.router.RackGet(p.Name)
	if err != nil {
		return err
	}

	activity := map[string]time.Time{}

	for _, h := range r.Hosts {
		parts := strings.Split(h.Hostname, ".")

		if len(parts) < 2 {
			continue
		}

		app := parts[len(parts)-1]

		if h.Activity.After(activity[app]) {
			activity[app] = h.Activity
		}
	}

	for app, latest := range activity {
		log.Logf("app=%s latest=%s", app, latest)

		if latest.Before(time.Now().UTC().Add(-60 * time.Minute)) {
			if err := p.AppUpdate(app, structs.AppUpdateOptions{Sleep: options.Bool(true)}); err != nil {
				return err
			}
		}
	}

	return nil
}

var serviceEndpoints = map[string]int{
	"http":  80,
	"https": 443,
}

func (p *Provider) route(app string) error {
	m, _, err := helpers.AppManifest(p, app)
	if err != nil {
		return err
	}

	// create host stubs for every app if they dont exist
	for _, s := range m.Services {
		host := fmt.Sprintf("%s.%s", s.Name, app)

		if err := p.router.HostCreate(p.Name, host); err != nil {
			return err
		}

		if s.Port.Port == 0 {
			continue
		}

		tc, err := containersByLabels(map[string]string{
			"convox.rack":    p.Name,
			"convox.app":     app,
			"convox.service": s.Name,
		})
		if err != nil {
			return err
		}

		targets := map[int][]string{}

		for _, c := range tc {
			for p, t := range c.Listeners {
				if targets[p] == nil {
					targets[p] = []string{}
				}
				targets[p] = append(targets[p], fmt.Sprintf("%s://%s", s.Port.Scheme, t))
			}
		}

		for proto, port := range serviceEndpoints {
			e, err := p.router.EndpointGet(p.Name, host, port)
			if err != nil {
				e, err = p.router.EndpointCreate(p.Name, host, proto, port)
				if err != nil {
					return err
				}
			}

			missing := diff(targets[s.Port.Port], e.Targets)
			extra := diff(e.Targets, targets[s.Port.Port])

			for _, t := range missing {
				if err := p.router.TargetAdd(p.Name, host, port, t); err != nil {
					return err
				}
			}

			for _, t := range extra {
				if err := p.router.TargetRemove(p.Name, host, port, t); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func diffContainers(a, b []container) []container {
	diff := []container{}

	for _, aa := range a {
		found := false

		for _, cc := range b {
			if reflect.DeepEqual(aa.Labels, cc.Labels) {
				found = true
				break
			}
		}

		if !found {
			diff = append(diff, aa)
		}
	}

	return diff
}

func resourcePort(kind string) (int, error) {
	switch kind {
	case "mysql":
		return 3306, nil
	case "postgres":
		return 5432, nil
	case "redis":
		return 6379, nil
	}

	return 0, fmt.Errorf("unknown resource type: %s", kind)
}

func resourceURL(app, kind, name string) (string, error) {
	switch kind {
	case "mysql":
		return fmt.Sprintf("mysql://mysql:password@%s.resource.%s.convox:3306/app", name, app), nil
	case "postgres":
		return fmt.Sprintf("postgres://postgres:password@%s.resource.%s.convox:5432/app?sslmode=disable", name, app), nil
	case "redis":
		return fmt.Sprintf("redis://%s.resource.%s.convox:6379/0", name, app), nil
	}

	return "", fmt.Errorf("unknown resource type: %s", kind)
}

func (p *Provider) resourceVolumes(app, kind, name string) ([]string, error) {
	switch kind {
	case "mysql":
		return []string{fmt.Sprintf("%s/%s/resource/%s:/var/lib/mysql", p.Volume, app, name)}, nil
	case "postgres":
		return []string{fmt.Sprintf("%s/%s/resource/%s:/var/lib/postgresql/data", p.Volume, app, name)}, nil
	case "redis":
		return []string{}, nil
	}

	return []string{}, fmt.Errorf("unknown resource type: %s", kind)
}

func (p *Provider) resourceContainers(resources manifest.Resources, app, release string) ([]container, error) {
	cs := []container{}

	for _, r := range resources {
		rp, err := resourcePort(r.Type)
		if err != nil {
			return nil, err
		}

		vs, err := p.resourceVolumes(app, r.Type, r.Name)
		if err != nil {
			return nil, err
		}

		hostname := fmt.Sprintf("%s.resource.%s", r.Name, app)

		cs = append(cs, container{
			Name:     fmt.Sprintf("%s.%s.resource.%s", p.Name, app, r.Name),
			Hostname: hostname,
			// Targets: []containerTarget{
			//   containerTarget{FromScheme: "tcp", FromPort: rp, ToScheme: "tcp", ToPort: rp},
			// },
			Image:   fmt.Sprintf("convox/%s", r.Type),
			Volumes: vs,
			Port:    rp,
			Labels: map[string]string{
				"convox.rack":     p.Name,
				"convox.version":  p.Version,
				"convox.app":      app,
				"convox.release":  release,
				"convox.type":     "resource",
				"convox.name":     r.Name,
				"convox.hostname": hostname,
				"convox.resource": r.Type,
			},
		})
	}

	return cs, nil
}

func (p *Provider) serviceContainers(services manifest.Services, app, release string) ([]container, error) {
	cs := []container{}

	m, r, err := helpers.ReleaseManifest(p, app, release)
	if err != nil {
		return nil, err
	}

	for _, s := range services {
		cmd := []string{}

		if c := strings.TrimSpace(s.Command); c != "" {
			cmd = append(cmd, "sh", "-c", c)
		}

		env, err := m.ServiceEnvironment(s.Name)
		if err != nil {
			return nil, err
		}

		// copy the map so we can hold on to it
		e := map[string]string{}

		for k, v := range env {
			e[k] = v
		}

		// add resources
		for _, sr := range s.Resources {
			for _, r := range m.Resources {
				if r.Name == sr {
					u, err := resourceURL(app, r.Type, r.Name)
					if err != nil {
						return nil, err
					}

					e[fmt.Sprintf("%s_URL", strings.ToUpper(sr))] = u
				}
			}
		}

		vv, err := p.serviceVolumes(app, s.Volumes)
		if err != nil {
			return nil, err
		}

		hostname := fmt.Sprintf("%s.%s", s.Name, app)

		for i := 1; i <= s.Scale.Count.Min; i++ {
			c := container{
				Hostname: hostname,
				Name:     fmt.Sprintf("%s.%s.service.%s.%d", p.Name, app, s.Name, i),
				Image:    fmt.Sprintf("%s/%s/%s:%s", p.Name, app, s.Name, r.Build),
				Command:  cmd,
				Env:      e,
				Memory:   s.Scale.Memory,
				Volumes:  vv,
				Port:     s.Port.Port,
				Labels: map[string]string{
					"convox.rack":     p.Name,
					"convox.version":  p.Version,
					"convox.app":      app,
					"convox.release":  release,
					"convox.type":     "service",
					"convox.name":     s.Name,
					"convox.hostname": hostname,
					"convox.service":  s.Name,
					"convox.index":    fmt.Sprintf("%d", i),
					"convox.port":     strconv.Itoa(s.Port.Port),
					"convox.scheme":   s.Port.Scheme,
				},
			}

			// if c.Port != 0 {
			//   c.Targets = []containerTarget{
			//     containerTarget{FromScheme: "http", FromPort: 80, ToScheme: s.Port.Scheme, ToPort: s.Port.Port},
			//     containerTarget{FromScheme: "https", FromPort: 443, ToScheme: s.Port.Scheme, ToPort: s.Port.Port},
			//   }
			// }

			cs = append(cs, c)
		}
	}

	return cs, nil
}
