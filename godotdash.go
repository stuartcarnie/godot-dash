package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unsafe"

	"github.com/PuerkitoBio/goquery"
	css "github.com/andybalholm/cascadia"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/spf13/cobra"
	"github.com/stuartcarnie/godotdash/pkg/parallel"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	cmd = &cobra.Command{
		Use:   "godot-dash",
		Short: "Godot Dash is a CLI tool for converting godot-docs to a Dash docset",
		RunE:  process,
	}
	// arguments
	noDB       bool
	noClasses  bool
	docsPath   string
	docsetPath string
	pathFilter regexFlag
)

type regexFlag struct{ re *regexp.Regexp }

func (r *regexFlag) String() string {
	if r.re == nil {
		return ""
	}
	return r.re.String()
}

func (r *regexFlag) Set(s string) error {
	re, err := regexp.Compile(s)
	if err != nil {
		return err
	}
	r.re = re
	return nil
}

func (r *regexFlag) Type() string {
	return "REGEX"
}

func init() {
	cmd.Flags().StringVar(&docsPath, "docs-path", "", "The path to the godot-docs source")
	cmd.Flags().StringVar(&docsetPath, "docset-path", "", "The base path to the Godot.docset")
	cmd.Flags().BoolVar(&noDB, "no-db", false, "Do not create the database (TESTING)")
	cmd.Flags().BoolVar(&noClasses, "no-classes", false, "Do not process classes (TESTING)")
	cmd.Flags().Var(&pathFilter, "path-filter", "A regex pattern to filter the paths to process (TESTING)")
	_ = cobra.MarkFlagRequired(cmd.Flags(), "docs-path")
	_ = cobra.MarkFlagRequired(cmd.Flags(), "docset-path")
}

func main() {
	_ = cmd.Execute()
}

const (
	workers   = 8
	batchSize = 1500
)

type SearchIndex struct {
	ID   int64  `gorm:"primaryKey;column:id"`
	Name string `gorm:"column:name;uniqueIndex:anchor"`
	Type string `gorm:"column:type;uniqueIndex:anchor"`
	Path string `gorm:"column:path;uniqueIndex:anchor"`
}

func (si SearchIndex) TableName() string {
	return "searchIndex"
}

var (
	wyNavSide        = css.MustCompile("nav.wy-nav-side")
	rstVersions      = css.MustCompile("div.rst-versions")
	wyNavContentWrap = css.MustCompile("section.wy-nav-content-wrap")
	hereBeDragons    = css.MustCompile("div.admonition-grid")
)

func cleanupDocument(top *html.Node, doc *goquery.Document) {
	// remove side nav bar
	{
		node := wyNavSide.MatchFirst(top)
		node.Parent.RemoveChild(node)
	}

	// remove versions
	{
		node := rstVersions.MatchFirst(top)
		node.Parent.RemoveChild(node)
	}

	// remove class from main section
	{
		res := doc.FindMatcher(wyNavContentWrap)
		res.RemoveAttr("class")
	}

	// remove "Attention: Here be dragons"
	{
		node := hereBeDragons.MatchFirst(top)
		node.Parent.RemoveChild(node)
	}
}

var (
	db         *gorm.DB
	targetPath string // targetPath is the Documents directory in the target docset
	// common selectors
	selHead  = css.MustCompile("head")
	selTitle = css.MustCompile("h1")
)

func process(cmd *cobra.Command, args []string) error {
	// Open the database
	dbFilename := filepath.Join(docsetPath, "Contents/Resources/docSet.dsidx")
	dsn := fmt.Sprintf("%s?_busy_timeout=", dbFilename)
	var err error
	if noDB == false {
		db, err = gorm.Open(sqlite.Open(dsn), &gorm.Config{
			CreateBatchSize: batchSize,
		})
		if err != nil {
			return fmt.Errorf("failed to open database: %w", err)
		}

		db.Exec("PRAGMA synchronous = OFF; PRAGMA JOURNAL_MODE = memory")

		migrator := db.Migrator()
		_ = migrator.DropTable(&SearchIndex{})
		_ = migrator.AutoMigrate(&SearchIndex{})
	}

	// start with index.html
	path := filepath.Join(docsPath, "index.html")
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	root, err := html.Parse(f)
	if err != nil {
		return fmt.Errorf("failed to parse HTML: %w", err)
	}

	targetPath = filepath.Join(docsetPath, "Contents/Resources/Documents")

	if noClasses == false {
		err = processClassesIndex()
		if err != nil {
			return err
		}
	}

	err = processGuides(root)
	if err != nil {
		return err
	}

	doc := goquery.NewDocumentFromNode(root)

	err = writeHTML(filepath.Join(targetPath, "index.html"), root, doc)
	if err != nil {
		return err
	}

	err = os.WriteFile(filepath.Join(docsetPath, "Contents/Resources/Documents/_static/css/dev.css"), []byte(devCss), 0644)
	if err != nil {
		return errors.Wrap(err, "failed to write dev.css")
	}

	err = os.WriteFile(filepath.Join(docsetPath, "Contents/Info.plist"), []byte(infoPlist), 0644)
	if err != nil {
		return errors.Wrap(err, "failed to write Info.plist")
	}

	// copy icon.png to the docset

	return nil
}

func writeRows(rows []SearchIndex) {
	if db == nil {
		return
	}

	db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "name"}, {Name: "type"}, {Name: "path"}},
		DoNothing: true,
	}).Create(rows)
}

func processClassesIndex() (err error) {
	slog.Info("Process classes")

	// open class index file
	path := filepath.Join(docsPath, "classes/index.html")
	f, err := os.Open(path)
	if err != nil {
		slog.Error("Failed to open file.", "error", err)
		return err
	}
	defer func() { _ = f.Close() }()
	root, err := html.Parse(f)
	if err != nil {
		slog.Error("Failed to parse HTML.", "error", err)
		return err
	}
	doc := goquery.NewDocumentFromNode(root)

	// globals
	nodes := doc.Find("section#globals li.toctree-l1 > a")
	err = processClasses(nodes, "Global")
	if err != nil {
		return err
	}

	// nodes
	nodes = doc.Find("section#nodes li.toctree-l1 > a")
	err = processClasses(nodes, "Class")
	if err != nil {
		return err
	}

	// resources
	nodes = doc.Find("section#resources li.toctree-l1 > a")
	err = processClasses(nodes, "Resource")
	if err != nil {
		return err
	}

	// other-objects
	nodes = doc.Find("section#other-objects li.toctree-l1 > a")
	err = processClasses(nodes, "Object")
	if err != nil {
		return err
	}

	// types
	nodes = doc.Find("section#variant-types li.toctree-l1 > a")
	err = processClasses(nodes, "Type")
	if err != nil {
		return err
	}

	return nil
}

func processClasses(sel *goquery.Selection, etype string) error {
	type inputData struct {
		FilePath string
		HRef     string
		Sel      *goquery.Selection
	}

	var classes []inputData
	sel.Each(func(i int, s *goquery.Selection) {
		ref, ok := s.Attr("href")
		if !ok {
			return
		}

		fileUrl, err := url.Parse(ref)
		if err != nil {
			slog.Error("Failed to parse URL.", "url", ref, "error", err)
			return
		}
		// prefix the class with "classes/"
		fileUrl.Path = filepath.Join("classes", fileUrl.Path)
		// update ref variable
		ref = fileUrl.String()
		classes = append(classes, inputData{
			FilePath: fileUrl.Path,
			HRef:     ref,
			Sel:      s,
		})
	})

	type class struct {
		Name string
		Path string
		Rows []SearchIndex
	}

	classData := make([]class, len(classes))

	var (
		selDescription      = css.MustCompile("section.classref-introduction-group#description > h2")
		selTutorials        = css.MustCompile("section.classref-introduction-group#tutorials > h2")
		selTutorialItems    = css.MustCompile("a.reference.internal")
		selProperties       = css.MustCompile("section.classref-reftable-group#properties > h2")
		selPropertyItems    = css.MustCompile("tr td:nth-child(2) a.reference.internal")
		selConstructors     = css.MustCompile("section.classref-reftable-group#constructors > h2")
		selConstructorItems = css.MustCompile("tr td:nth-child(2) a.reference.internal")
		selMethods          = css.MustCompile("section.classref-reftable-group#methods > h2")
		selMethodItems      = css.MustCompile("tr td:nth-child(2) a:nth-child(1).reference.internal")
		selOperators        = css.MustCompile("section.classref-reftable-group#operators > h2")
		selOperatorItems    = css.MustCompile("tr td:nth-child(2) a.reference.internal")
		selSignals          = css.MustCompile("section.classref-descriptions-group#signals > h2")
		selSignalItems      = css.MustCompile("p.classref-signal")
		selEnumerations     = css.MustCompile("section.classref-descriptions-group#enumerations > h2")
		selEnumerationItems = css.MustCompile("p.classref-enumeration")
		selConstants        = css.MustCompile("section.classref-descriptions-group#constants > h2")
		selConstantItems    = css.MustCompile("p.classref-constant")
	)

	// Process all classes
	err := parallel.For(len(classes), func(i, _ int) error {
		data := &classes[i]
		cd := &classData[i]
		{
			slog.Info("Processing file.", "class", data.Sel.Text(), "path", data.FilePath)
			cd.Name = data.Sel.Text()
			cd.Path = data.HRef
		}

		path := filepath.Join(docsPath, data.FilePath)
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("failed to open file: %w", err)
		}
		defer func() { _ = f.Close() }()

		top, err := html.Parse(f)
		if err != nil {
			return fmt.Errorf("failed to parse HTML: %w", err)
		}
		doc := goquery.NewDocumentFromNode(top)

		// head
		headNode := selHead.MatchFirst(top)

		// Class name
		var className string
		{
			n := doc.FindMatcher(selTitle).First()
			className = strings.TrimRight(n.Text(), "¶\uF0C1")
			link, a, _ := newSectionHeaderLink(className, "Class")
			headNode.AppendChild(link)
			n.Get(0).Parent.InsertBefore(a, n.Get(0))

			link, a, _ = newSectionItemLink(className, "Class")
			headNode.AppendChild(link)
			n.Get(0).Parent.InsertBefore(a, n.Get(0))
		}

		// Description
		if n := doc.FindMatcher(selDescription).First(); n.Length() > 0 {
			descriptionText := strings.TrimRight(n.Text(), "¶\uF0C1")
			link, a, _ := newSectionHeaderLink(descriptionText, "Section")
			headNode.AppendChild(link)
			n.Get(0).Parent.InsertBefore(a, n.Get(0))

			link, a, _ = newSectionItemLink(descriptionText, "Section")
			headNode.AppendChild(link)
			n.Get(0).Parent.InsertBefore(a, n.Get(0))
		}

		// tutorials
		if n := doc.FindMatcher(selTutorials).First(); n.Length() > 0 {
			// make sure we have some internal links
			if items := n.Parent().FindMatcher(selTutorialItems); items.Length() > 0 {
				tutorialName := strings.TrimRight(n.Text(), "¶\uF0C1")
				link, a, _ := newSectionHeaderLink(tutorialName, "Guide")
				headNode.AppendChild(link)
				n.Get(0).Parent.InsertBefore(a, n.Get(0))

				items.Each(func(i int, s *goquery.Selection) {
					text := s.Text()
					link, a, _ := newSectionItemLink(text, "Guide")
					headNode.AppendChild(link)
					s.Get(0).Parent.InsertBefore(a, s.Get(0))
				})
			}
		}

		refTable := func(h2, items css.Selector, etype, descriptionsID string) {
			if n := doc.FindMatcher(h2).First(); n.Length() > 0 {
				// make sure we have items in the table
				if items := n.Parent().FindMatcher(items); items.Length() > 0 {
					text := strings.TrimRight(n.Text(), "¶\uF0C1") // Properties table name
					link, a, _ := newSectionHeaderLink(text, etype)
					headNode.AppendChild(link)
					n.Get(0).Parent.InsertBefore(a, n.Get(0))

					items.Each(func(i int, s *goquery.Selection) {
						// now find the id of the link, so that the TOC can link to it
						if id, ok := s.Attr("href"); ok {
							desc := doc.Find(fmt.Sprintf("body section#%s %s", descriptionsID, id)).First()
							if desc.Length() > 0 {
								s = s.Parent() // we want the complete text
								itemName := s.Text()
								link, a, target := newSectionItemLink(itemName, etype)
								headNode.AppendChild(link)
								desc.Get(0).Parent.InsertBefore(a, desc.Get(0))

								cd.Rows = append(cd.Rows, SearchIndex{
									Name: itemName,
									Type: etype,
									Path: makeSearchIndexPath(data.FilePath, itemName, itemName, className, target),
								})
							}
						}
					})
				}
			}
		}

		// These use the reftable class
		refTable(selProperties, selPropertyItems, "Property", "property-descriptions")
		refTable(selConstructors, selConstructorItems, "Constructor", "constructor-descriptions")
		refTable(selMethods, selMethodItems, "Method", "method-descriptions")
		refTable(selOperators, selOperatorItems, "Operator", "operator-descriptions")

		// signals
		if n := doc.FindMatcher(selSignals).First(); n.Length() > 0 {
			// make sure we have items
			if items := n.Parent().FindMatcher(selSignalItems); items.Length() > 0 {
				signalsText := strings.TrimRight(n.Text(), "¶\uF0C1")
				link, a, _ := newSectionHeaderLink(signalsText, "Signal")
				headNode.AppendChild(link)
				n.Get(0).Parent.InsertBefore(a, n.Get(0))

				items.Each(func(i int, s *goquery.Selection) {
					signalName := s.Find("strong").Text()
					if signalName == "" {
						return
					}

					link, a, target := newSectionItemLink(signalName, "Signal")
					headNode.AppendChild(link)
					s.Get(0).Parent.InsertBefore(a, s.Get(0))
					cd.Rows = append(cd.Rows, SearchIndex{
						Name: signalName,
						Type: "Signal",
						Path: makeSearchIndexPath(data.FilePath, signalName, signalName, className, target),
					})
				})
			}
		}

		// enumerations
		// Here we extract the enumeration name, and then find all the enum variants
		// and format them as <enum>.<variant>
		if enumNode := doc.FindMatcher(selEnumerations).First(); enumNode.Length() > 0 {
			// make sure we have items
			if items := enumNode.Parent().FindMatcher(selEnumerationItems); items.Length() > 0 {
				enumsText := strings.TrimRight(enumNode.Text(), "¶\uF0C1")
				link, a, _ := newSectionHeaderLink(enumsText, "Enum")
				headNode.AppendChild(link)
				enumNode.Get(0).Parent.InsertBefore(a, enumNode.Get(0))

				items.Each(func(i int, s *goquery.Selection) {
					id, ok := s.Attr("id")
					if !ok {
						return
					}
					enumName := s.Find("strong").Text()
					if enumName == "" {
						return
					}

					link, a, target := newSectionItemLink(enumName, "Enum")
					headNode.AppendChild(link)
					s.Get(0).Parent.InsertBefore(a, s.Get(0))
					cd.Rows = append(cd.Rows, SearchIndex{
						Name: enumName,
						Type: "Enum",
						Path: makeSearchIndexPath(data.FilePath, enumName, enumName, className, target),
					})

					// now find all enum variants
					constants := s.Parent().Find(fmt.Sprintf("p.classref-enumeration-constant > a[href=\"#%s\"]", id))
					constants.Each(func(i int, s *goquery.Selection) {
						nameNode := s.Parent().Find("strong")
						constantName := nameNode.Text()
						if constantName == "" {
							return
						}
						constantName = enumName + "." + constantName

						link, a, target := newSectionItemLink(constantName, "Enum")
						headNode.AppendChild(link)
						nameNode.Get(0).InsertBefore(a, nameNode.Get(0))
						cd.Rows = append(cd.Rows, SearchIndex{
							Name: constantName,
							Type: "Enum",
							Path: makeSearchIndexPath(data.FilePath, constantName, constantName, className, target),
						})
					})
				})
			}
		}

		// constants
		if n := doc.FindMatcher(selConstants).First(); n.Length() > 0 {
			// make sure we have items
			if items := n.Parent().FindMatcher(selConstantItems); items.Length() > 0 {
				constantsText := strings.TrimRight(n.Text(), "¶\uF0C1")
				link, a, _ := newSectionHeaderLink(constantsText, "Constant")
				headNode.AppendChild(link)
				n.Get(0).Parent.InsertBefore(a, n.Get(0))

				items.Each(func(i int, s *goquery.Selection) {
					constantName := s.Find("strong").Text()
					if constantName == "" {
						return
					}

					link, a, target := newSectionItemLink(constantName, "Constant")
					headNode.AppendChild(link)
					s.Get(0).Parent.InsertBefore(a, s.Get(0))
					cd.Rows = append(cd.Rows, SearchIndex{
						Name: constantName,
						Type: "Constant",
						Path: makeSearchIndexPath(data.FilePath, constantName, constantName, className, target),
					})
				})
			}
		}

		writeRows(cd.Rows)

		return writeHTML(filepath.Join(targetPath, data.FilePath), top, doc)
	})

	if err != nil {
		return err
	}

	rows := lo.Map(classData, func(c class, i int) SearchIndex {
		return SearchIndex{
			Name: c.Name,
			Type: etype,
			Path: c.Path,
		}
	})

	writeRows(rows)

	return nil
}

func processGuides(root *html.Node) error {
	sel := css.MustCompile("li.toctree-l1 > a, li.toctree-l2 > a, li.toctree-l3 > a")
	doc := goquery.NewDocumentFromNode(root)

	type document struct {
		Title      string
		GroupTitle string // set if this document is part of a group
		FilePath   string
		HRef       string
	}

	// docFileSet is the unique set of all documents to process
	docFileSet := make(map[string]struct{})
	// map the path to a document to its title
	pathTitleMap := make(map[string]string)
	input := make([]document, 0, doc.Length()) // reserve at least the maximum number of documents
	doc.FindMatcher(sel).Each(func(i int, s *goquery.Selection) {
		ref, ok := s.Attr("href")
		if !ok {
			return
		}

		fileUrl, err := url.Parse(ref)
		if err != nil {
			slog.Error("Failed to parse URL.", "url", ref, "error", err)
			return
		}

		// skip classes
		if strings.HasPrefix(fileUrl.Path, "classes/") {
			return
		}

		fileUrl.Fragment = ""

		if _, ok := docFileSet[fileUrl.Path]; ok {
			// only process one copy of the file
			return
		}
		docFileSet[fileUrl.Path] = struct{}{}

		title := s.Text()

		pathTitleMap[fileUrl.Path] = title

		input = append(input, document{
			Title:    title,
			FilePath: fileUrl.Path,
			HRef:     fileUrl.String(),
		})
	})

	// find group titles, which means looking to see if there is an index.html,
	// in the same path as the current document and using that title as the group
	for i := range input {
		d := &input[i]
		dir := filepath.Dir(d.FilePath)
		index := filepath.Join(dir, "index.html")
		if _, ok := docFileSet[index]; ok {
			d.GroupTitle = pathTitleMap[index]
		}
	}

	var (
		mainHeader    = css.MustCompile("section > h1")
		sectionHeader = css.MustCompile("section > h2")
	)

	err := parallel.For(len(input), func(i, _ int) error {
		data := &input[i]
		slog.Info("Processing file.", "guide", data.Title, "group", data.GroupTitle, "path", data.FilePath)

		path := filepath.Join(docsPath, data.FilePath)
		f, err := os.Open(path)
		if err != nil {
			slog.Error("Failed to open file.", "error", err)
			// skip it
			return nil
		}
		defer func() { _ = f.Close() }()

		top, err := html.Parse(f)
		if err != nil {
			return fmt.Errorf("failed to parse HTML: %w", err)
		}
		doc := goquery.NewDocumentFromNode(top)

		// head
		headNode := selHead.MatchFirst(top)

		h1 := doc.FindMatcher(mainHeader).First()

		link, a, _ := newSectionHeaderLink(data.Title, "Section")
		headNode.AppendChild(link)
		h1.Get(0).Parent.InsertBefore(a, h1.Get(0))

		// add all the sections
		doc.FindMatcher(sectionHeader).Each(func(i int, s *goquery.Selection) {
			sectionName := strings.TrimRight(s.Text(), "¶\uF0C1")
			link, a, _ := newSectionItemLink(sectionName, "Section")
			headNode.AppendChild(link)
			s.Get(0).Parent.InsertBefore(a, s.Get(0))
		})

		return writeHTML(filepath.Join(targetPath, data.FilePath), top, doc)
	})

	if err != nil {
		return err
	}

	rows := lo.Map(input, func(d document, i int) SearchIndex {
		return SearchIndex{
			Name: d.Title,
			Type: "Guide",
			Path: makeSearchIndexPath(d.FilePath, d.Title, d.Title, d.GroupTitle, ""),
		}
	})

	writeRows(rows)

	return nil
}

func newSectionHeaderLink(name, etype string) (headLink *html.Node, a *html.Node, target string) {
	return newSectionLink(name, etype, true)
}

func newSectionItemLink(name, etype string) (headLink *html.Node, a *html.Node, target string) {
	return newSectionLink(name, etype, false)
}

func newSectionLink(name, etype string, isSectionHeader bool) (headLink *html.Node, a *html.Node, target string) {
	name = strings.Replace(url.QueryEscape(name), "+", "%20", -1)
	var isSection int
	if isSectionHeader {
		isSection = 1
	}
	target = fmt.Sprintf("//dash_ref/%s/%s/%d", etype, name, isSection)

	return &html.Node{
			Type:     html.ElementNode,
			DataAtom: atom.Link,
			Data:     atom.Link.String(),
			Attr: []html.Attribute{
				{Key: "href", Val: target},
			},
		}, &html.Node{
			Type:     html.ElementNode,
			DataAtom: atom.A,
			Data:     atom.A.String(),
			Attr: []html.Attribute{
				{Key: "class", Val: "dashAnchor"},
				{Key: "name", Val: target},
			},
		}, target
}

func makeSearchIndexPath(docPath, entryName, origName, desc, target string) string {
	entryName = url.PathEscape(entryName)
	origName = url.PathEscape(origName)
	return fmt.Sprintf("<dash_entry_name=%s><dash_entry_originalName=%s><dash_entry_menuDescription=%s>%s#%s", entryName, origName, desc, docPath, target)
}

var devCss = `
/**
 * CSS tweaks that are only added outside ReadTheDocs (i.e. when built locally).
 */

/* Re-add default red boxes around Pygments errors */
.highlight .err {
    border: 1px solid #FF0000;
}


/**
 * Adjust the layout for Dash 
 */

.wy-nav-content {
    max-width: none;
}

.wy-nav-content-wrap {
    margin-left: unset;
}

.wy-body-for-nav {
    max-width: unset;
}
`

var infoPlist = `
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key>
	<string>godot</string>
	<key>CFBundleName</key>
	<string>Godot</string>
	<key>DocSetPlatformFamily</key>
	<string>godot</string>
	<key>DashDocSetFallbackURL</key>
	<string>https://docs.godotengine.org/en/stable/</string>
	<key>DashDocSetFamily</key>
	<string>dashtoc3</string>
	<key>isDashDocset</key>
	<true/>
	<key>isJavaScriptEnabled</key>
	<true/>
	<key>dashIndexFilePath</key>
	<string>index.html</string>
</dict>
</plist>
`

func writeHTML(dest string, root *html.Node, doc *goquery.Document) error {
	cleanupDocument(root, doc)

	dir := filepath.Dir(dest)
	_ = os.MkdirAll(dir, 0755)
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	var contentBytes bytes.Buffer
	err = html.Render(&contentBytes, root)
	if err != nil {
		return errors.Wrapf(err, "failed to render HTML for %s", dest)
	}
	b := contentBytes.Bytes()
	content := unsafe.String(&b[0], len(b))
	if hasHTMLEntities(content) {
		content = encodeHTMLEntities(content)
	}
	_, err = out.WriteString(content)
	return err
}

func hasHTMLEntities(orig string) bool {
	for _, c := range orig {
		if _, ok := pointToEntity[c]; ok {
			return true
		}
	}
	return false

}

func encodeHTMLEntities(orig string) string {
	var escaped bytes.Buffer
	for _, c := range orig {
		if pointToEntity[c] == "" {
			escaped.WriteRune(c)
		} else {
			escaped.WriteString(pointToEntity[c])
		}
	}

	return escaped.String()
}

var pointToEntity = map[rune]string{
	8704: "&forall;",
	8194: "&ensp;",
	8195: "&emsp;",
	8709: "&empty;",
	8711: "&nabla;",
	8712: "&isin;",
	8201: "&thinsp;",
	8715: "&ni;",
	8204: "&zwnj;",
	8205: "&zwj;",
	8206: "&lrm;",
	8719: "&prod;",
	8721: "&sum;",
	8722: "&minus;",
	8211: "&ndash;",
	8212: "&mdash;",
	8727: "&lowast;",
	8216: "&lsquo;",
	8217: "&rsquo;",
	8730: "&radic;",
	175:  "&macr;",
	8220: "&ldquo;",
	8221: "&rdquo;",
	8222: "&bdquo;",
	8224: "&dagger;",
	8225: "&Dagger;",
	8226: "&bull;",
	8230: "&hellip;",
	8743: "&and;",
	8744: "&or;",
	8745: "&cap;",
	8746: "&cup;",
	8747: "&int;",
	8240: "&permil;",
	8242: "&prime;",
	8243: "&Prime;",
	8756: "&there4;",
	8713: "&notin;",
	8249: "&lsaquo;",
	8250: "&rsaquo;",
	8764: "&sim;",
	// 62:   "&gt;",	// this is already encoded for us
	8629: "&crarr;",
	9824: "&spades;",
	8260: "&frasl;",
	8773: "&cong;",
	8776: "&asymp;",
	8207: "&rlm;",
	9829: "&hearts;",
	8800: "&ne;",
	8801: "&equiv;",
	9827: "&clubs;",
	8804: "&le;",
	8805: "&ge;",
	9830: "&diams;",
	// 38:   "&amp;",	// this is already encoded for us
	8834: "&sub;",
	8835: "&sup;",
	8836: "&nsub;",
	8838: "&sube;",
	8839: "&supe;",
	8853: "&oplus;",
	8855: "&otimes;",
	8734: "&infin;",
	8218: "&sbquo;",
	8901: "&sdot;",
	160:  "&nbsp;",
	161:  "&iexcl;",
	162:  "&cent;",
	163:  "&pound;",
	164:  "&curren;",
	8869: "&perp;",
	166:  "&brvbar;",
	167:  "&sect;",
	168:  "&uml;",
	169:  "&copy;",
	170:  "&ordf;",
	171:  "&laquo;",
	8364: "&euro;",
	173:  "&shy;",
	174:  "&reg;",
	8733: "&prop;",
	176:  "&deg;",
	177:  "&plusmn;",
	178:  "&sup2;",
	179:  "&sup3;",
	180:  "&acute;",
	181:  "&micro;",
	182:  "&para;",
	183:  "&middot;",
	184:  "&cedil;",
	185:  "&sup1;",
	186:  "&ordm;",
	187:  "&raquo;",
	188:  "&frac14;",
	189:  "&frac12;",
	190:  "&frac34;",
	191:  "&iquest;",
	192:  "&Agrave;",
	193:  "&Aacute;",
	194:  "&Acirc;",
	195:  "&Atilde;",
	196:  "&Auml;",
	197:  "&Aring;",
	198:  "&AElig;",
	199:  "&Ccedil;",
	200:  "&Egrave;",
	201:  "&Eacute;",
	202:  "&Ecirc;",
	203:  "&Euml;",
	204:  "&Igrave;",
	// 34:   "&quot;",	// this is already encoded
	206:  "&Icirc;",
	207:  "&Iuml;",
	208:  "&ETH;",
	209:  "&Ntilde;",
	210:  "&Ograve;",
	211:  "&Oacute;",
	212:  "&Ocirc;",
	213:  "&Otilde;",
	214:  "&Ouml;",
	215:  "&times;",
	216:  "&Oslash;",
	217:  "&Ugrave;",
	218:  "&Uacute;",
	219:  "&Ucirc;",
	220:  "&Uuml;",
	221:  "&Yacute;",
	222:  "&THORN;",
	223:  "&szlig;",
	224:  "&agrave;",
	225:  "&aacute;",
	226:  "&acirc;",
	227:  "&atilde;",
	228:  "&auml;",
	229:  "&aring;",
	230:  "&aelig;",
	231:  "&ccedil;",
	232:  "&egrave;",
	205:  "&Iacute;",
	234:  "&ecirc;",
	235:  "&euml;",
	236:  "&igrave;",
	8658: "&rArr;",
	238:  "&icirc;",
	239:  "&iuml;",
	240:  "&eth;",
	241:  "&ntilde;",
	242:  "&ograve;",
	243:  "&oacute;",
	244:  "&ocirc;",
	245:  "&otilde;",
	246:  "&ouml;",
	247:  "&divide;",
	248:  "&oslash;",
	249:  "&ugrave;",
	250:  "&uacute;",
	251:  "&ucirc;",
	252:  "&uuml;",
	253:  "&yacute;",
	254:  "&thorn;",
	255:  "&yuml;",
	172:  "&not;",
	8968: "&lceil;",
	8969: "&rceil;",
	8970: "&lfloor;",
	8971: "&rfloor;",
	8465: "&image;",
	8472: "&weierp;",
	8476: "&real;",
	8482: "&trade;",
	732:  "&tilde;",
	9002: "&rang;",
	8736: "&ang;",
	402:  "&fnof;",
	8706: "&part;",
	8501: "&alefsym;",
	710:  "&circ;",
	338:  "&OElig;",
	339:  "&oelig;",
	352:  "&Scaron;",
	353:  "&scaron;",
	8593: "&uarr;",
	// 60:   "&lt;",	// this is already encoded for us
	8594: "&rarr;",
	8707: "&exist;",
	8595: "&darr;",
	8254: "&oline;",
	233:  "&eacute;",
	376:  "&Yuml;",
	916:  "&Delta;",
	237:  "&iacute;",
	8592: "&larr;",
	913:  "&Alpha;",
	914:  "&Beta;",
	915:  "&Gamma;",
	8596: "&harr;",
	917:  "&Epsilon;",
	918:  "&Zeta;",
	919:  "&Eta;",
	920:  "&Theta;",
	921:  "&Iota;",
	922:  "&Kappa;",
	923:  "&Lambda;",
	924:  "&Mu;",
	925:  "&Nu;",
	926:  "&Xi;",
	927:  "&Omicron;",
	928:  "&Pi;",
	929:  "&Rho;",
	931:  "&Sigma;",
	932:  "&Tau;",
	933:  "&Upsilon;",
	934:  "&Phi;",
	935:  "&Chi;",
	936:  "&Psi;",
	937:  "&Omega;",
	945:  "&alpha;",
	946:  "&beta;",
	947:  "&gamma;",
	948:  "&delta;",
	949:  "&epsilon;",
	950:  "&zeta;",
	951:  "&eta;",
	952:  "&theta;",
	953:  "&iota;",
	954:  "&kappa;",
	955:  "&lambda;",
	956:  "&mu;",
	957:  "&nu;",
	958:  "&xi;",
	959:  "&omicron;",
	960:  "&pi;",
	961:  "&rho;",
	962:  "&sigmaf;",
	963:  "&sigma;",
	964:  "&tau;",
	965:  "&upsilon;",
	966:  "&phi;",
	967:  "&chi;",
	968:  "&psi;",
	969:  "&omega;",
	9674: "&loz;",
	8656: "&lArr;",
	977:  "&thetasym;",
	978:  "&upsih;",
	8659: "&dArr;",
	8660: "&hArr;",
	982:  "&piv;",
	165:  "&yen;",
	8657: "&uArr;",
	9001: "&lang;",
}
