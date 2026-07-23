package appdeploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
)

type Project struct {
	Name    string
	Domains []string
}

func ParseProjects(reader io.Reader) ([]Project, error) {
	var raw []map[string]any
	if err := json.NewDecoder(io.LimitReader(reader, 4*1024*1024)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse Cloudflare Pages projects: %w", err)
	}
	projects := make([]Project, 0, len(raw))
	for _, item := range raw {
		name := stringField(item, "Project Name", "name", "project_name")
		if name == "" {
			continue
		}
		domainText := stringField(item, "Project Domains", "domains")
		var domains []string
		for _, domain := range strings.Split(domainText, ",") {
			if domain = strings.ToLower(strings.TrimSpace(domain)); domain != "" {
				domains = append(domains, domain)
			}
		}
		projects = append(projects, Project{Name: name, Domains: domains})
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	return projects, nil
}

func MatchingProjects(projects []Project, origin string) ([]Project, error) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Hostname() == "" {
		return nil, errors.New("app origin is invalid")
	}
	host := strings.ToLower(parsed.Hostname())
	var result []Project
	for _, project := range projects {
		for _, domain := range project.Domains {
			if domain == host {
				result = append(result, project)
				break
			}
		}
	}
	return result, nil
}

func ValidateProject(projects []Project, name, origin string) error {
	matches, err := MatchingProjects(projects, origin)
	if err != nil {
		return err
	}
	found := false
	for _, project := range projects {
		if project.Name == name {
			found = true
			break
		}
	}
	if !found {
		return errors.New("project is not available")
	}
	for _, project := range matches {
		if project.Name == name {
			return nil
		}
	}
	return fmt.Errorf("%s does not serve the configured app origin", name)
}

func stringField(value map[string]any, keys ...string) string {
	for _, key := range keys {
		switch field := value[key].(type) {
		case string:
			return strings.TrimSpace(field)
		case []any:
			var values []string
			for _, item := range field {
				if text, ok := item.(string); ok {
					values = append(values, text)
				}
			}
			return strings.Join(values, ",")
		}
	}
	return ""
}
