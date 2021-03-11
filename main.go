package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docopt/docopt-go"
	"github.com/kovetskiy/lorg"
	"github.com/kovetskiy/mark/pkg/confluence"
	"github.com/kovetskiy/mark/pkg/mark"
	"github.com/kovetskiy/mark/pkg/mark/includes"
	"github.com/kovetskiy/mark/pkg/mark/macro"
	"github.com/kovetskiy/mark/pkg/mark/stdlib"
	"github.com/reconquest/karma-go"
	"github.com/reconquest/pkg/log"
)

const (
	usage = `mark - a tool for updating Atlassian Confluence pages from markdown.

Docs: https://github.com/kovetskiy/mark

Usage:
  mark [options] [-u <username>] [-p <token>] [-k] [-l <url>] -f <file>
  mark [options] [-u <username>] [-p <password>] [-k] [-b <url>] -f <file>
  mark -v | --version
  mark -h | --help

Options:
  -u <username>        Use specified username for updating Confluence page.
  -p <token>           Use specified token for updating Confluence page.
                        Specify - as password to read password from stdin.
  -l <url>             Edit specified Confluence page.
                        If -l is not specified, file should contain metadata (see
                        above).
  -b --base-url <url>  Base URL for Confluence.
                        Alternative option for base_url config field.
  -f <file>            Use specified markdown file for converting to html.
  -k                   Lock page editing to current user only to prevent accidental
                        manual edits over Confluence Web UI.
  --drop-h1            Don't include H1 headings in Confluence output.
  --dry-run            Resolve page and ancestry, show resulting HTML and exit.
  --compile-only       Show resulting HTML and don't update Confluence page content.
  --minor-edit         Don't send notifications while updating Confluence page.
  --debug              Enable debug logs.
  --trace              Enable trace logs.
  --no-raw-attachments    Disable raw attachments processing (use 'attachment://' prefix) [default: false]
  --page-version <file> Check page version sequence.
  -h --help            Show this screen and call 911.
  -v --version         Show version.
`
)

func main() {
	args, err := docopt.Parse(usage, nil, true, "5.2.1", false)
	if err != nil {
		panic(err)
	}

	var (
		targetFile, _         = args["-f"].(string)
		compileOnly           = args["--compile-only"].(bool)
		dryRun                = args["--dry-run"].(bool)
		editLock              = args["-k"].(bool)
		dropH1                = args["--drop-h1"].(bool)
		minorEdit             = args["--minor-edit"].(bool)
		disableRawAttachments = args["--no-raw-attachments"].(bool)
		checkVersion, _       = args["--page-version"].(string)
	)

	if args["--debug"].(bool) {
		log.SetLevel(lorg.LevelDebug)
	}

	if args["--trace"].(bool) {
		log.SetLevel(lorg.LevelTrace)
	}

	config, err := LoadConfig(filepath.Join(os.Getenv("HOME"), ".config/mark"))
	if err != nil {
		log.Fatal(err)
	}

	creds, err := GetCredentials(args, config)
	if err != nil {
		log.Fatal(err)
	}

	api := confluence.NewAPI(creds.BaseURL, creds.Username, creds.Password)

	markdown, err := ioutil.ReadFile(targetFile)
	if err != nil {
		log.Fatal(err)
	}

	meta, markdown, err := mark.ExtractMeta(markdown)
	if err != nil {
		log.Fatal(err)
	}

	stdlib, err := stdlib.New(api)
	if err != nil {
		log.Fatal(err)
	}

	templates := stdlib.Templates

	var recurse bool

	for {
		templates, markdown, recurse, err = includes.ProcessIncludes(
			markdown,
			templates,
		)
		if err != nil {
			log.Fatal(err)
		}

		if !recurse {
			break
		}
	}

	macros, markdown, err := macro.ExtractMacros(markdown, templates)
	if err != nil {
		log.Fatal(err)
	}

	macros = append(macros, stdlib.Macros...)

	for _, macro := range macros {
		markdown, err = macro.Apply(markdown)
		if err != nil {
			log.Fatal(err)
		}
	}

	links, err := mark.ResolveRelativeLinks(api, meta, markdown, ".")
	if err != nil {
		log.Fatalf(err, "unable to resolve relative links")
	}

	markdown = mark.SubstituteLinks(markdown, links)

	if dryRun {
		compileOnly = true

		_, _, err := mark.ResolvePage(dryRun, api, meta)
		if err != nil {
			log.Fatalf(err, "unable to resolve page location")
		}
	}

	if compileOnly {
		fmt.Println(mark.CompileMarkdown(markdown, stdlib))
		os.Exit(0)
	}

	if creds.PageID != "" && meta != nil {
		log.Warning(
			`specified file contains metadata, ` +
				`but it will be ignored due specified command line URL`,
		)

		meta = nil
	}

	if creds.PageID == "" && meta == nil {
		log.Fatal(
			`specified file doesn't contain metadata ` +
				`and URL is not specified via command line ` +
				`or doesn't contain pageId GET-parameter`,
		)
	}

	var target *confluence.PageInfo

	if meta != nil {
		parent, page, err := mark.ResolvePage(dryRun, api, meta)
		if err != nil {
			log.Fatalf(
				karma.Describe("title", meta.Title).Reason(err),
				"unable to resolve page",
			)
		}

		if page == nil {
			page, err = api.CreatePage(meta.Space, parent, meta.Title, ``)
			if err != nil {
				log.Fatalf(
					err,
					"can't create page %q",
					meta.Title,
				)
			}
		}

		target = page
	} else {
		if creds.PageID == "" {
			log.Fatalf(nil, "URL should provide 'pageId' GET-parameter")
		}

		page, err := api.GetPageByID(creds.PageID)
		if err != nil {
			log.Fatalf(err, "unable to retrieve page by id")
		}

		target = page
	}

	if checkVersion != "" {
		check_version(target, checkVersion)
	}

	attaches, err := mark.ResolveAttachments(api, target, ".", meta.Attachments)
	if err != nil {
		log.Fatalf(err, "unable to create/update attachments")
	}

	markdown = mark.CompileAttachmentLinks(markdown, attaches, !disableRawAttachments)

	if dropH1 {
		log.Info("Leading H1 heading will be excluded from the Confluence output")
		markdown = mark.DropDocumentLeadingH1(markdown)
	}

	html := mark.CompileMarkdown(markdown, stdlib)

	{
		var buffer bytes.Buffer

		err := stdlib.Templates.ExecuteTemplate(
			&buffer,
			"ac:layout",
			struct {
				Layout string
				Body   string
			}{
				Layout: meta.Layout,
				Body:   html,
			},
		)
		if err != nil {
			log.Fatal(err)
		}

		html = buffer.String()
	}

	nextPageVersion, err := api.UpdatePage(target, html, minorEdit, meta.Labels)
	if err != nil {
		log.Fatal(err)
	}

	if editLock {
		log.Infof(
			nil,
			`edit locked on page %q by user %q to prevent manual edits`,
			target.Title,
			creds.Username,
		)

		err := api.RestrictPageUpdates(
			target,
			creds.Username,
		)
		if err != nil {
			log.Fatal(err)
		}
	}

	if checkVersion != "" {
		version_text := strconv.FormatInt(nextPageVersion, 10) + "\n"
		err := ioutil.WriteFile(checkVersion, []byte(version_text), 0644)
		if err != nil {
			log.Fatalf(err, "Unable to write page version file %s as %d", checkVersion, nextPageVersion)
		}
	}

	log.Infof(
		nil,
		"page successfully updated: version %d, url %s",
		nextPageVersion,
		creds.BaseURL+target.Links.Full,
	)

	fmt.Printf("version %d\n", nextPageVersion)
	fmt.Println(
		creds.BaseURL + target.Links.Full,
	)
}

func check_version(target *confluence.PageInfo, version_file string) {
	might_miss := (target.Version.Number == 1)
	data, err := ioutil.ReadFile(version_file)
	if err != nil {
		if might_miss {
			return
		}
		log.Fatalf(err, "Unable to load version file. Current page version is %d", target.Version.Number)
	}

	text := strings.TrimSpace(string(data))
	old_version, err := strconv.ParseInt(text, 10, 64)

	if err != nil {
		log.Fatalf(err, "Unable to parse version file")
	}

	if target.Version.Number != old_version {
		log.Fatalf(err, "Page version mismatch. Known version %d, Page version %d", old_version, target.Version.Number)
	}
}
