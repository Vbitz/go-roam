package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"sort"
	"strings"

	"olympos.io/encoding/edn"
)

var (
	inputFile  = flag.String("input", "", "A EDN format file exported from Roam Research to parse.")
	publishTag = flag.String("publishTag", "publish", "The tag in Roam Research to control extracting posts.")
)

type RoamSchema struct {
	Cardinality edn.Keyword `edn:"db/cardinality"`
	ValueType   edn.Keyword `edn:"db/valueType"`
	Unique      edn.Keyword `edn:"db/unique"`
}

type EntityId int64
type TransactionId int64

type Value struct {
	Attribute     edn.Keyword
	Value         any
	TransactionId TransactionId
}

func (v Value) String() string {
	return fmt.Sprintf("%s: %+v", v.Attribute, v.Value)
}

type Entity struct {
	graph   *RoamGraph
	blockId string

	Id     EntityId
	Values []Value
}

func (e Entity) Block() *Block {
	return e.graph.Blocks[e.blockId]
}

func (e Entity) String() string {
	return fmt.Sprintf("{%v: %+v}", e.Id, e.Values)
}

type Block struct {
	graph *RoamGraph
	ent   *Entity

	Id           string
	incomingRefs []*Entity
	children     []*Entity
}

func (b Block) String() string {
	return b.Id
}

func (b Block) Attr(attr edn.Keyword) []any {
	var ret []any
	for _, value := range b.ent.Values {
		if value.Attribute == attr {
			ret = append(ret, value.Value)
		}
	}
	return ret
}

func (b Block) Uid() string {
	uid := b.Attr(edn.Keyword("block/uid"))
	return uid[0].(string)
}

func (b Block) Text() string {
	uid := b.Attr(edn.Keyword("block/string"))
	return uid[0].(string)
}

func (b Block) Order() int64 {
	uid := b.Attr(edn.Keyword("block/order"))
	return uid[0].(int64)
}

func (b Block) Parents() []*Block {
	var ret []*Block

	for _, blockId := range b.Attr("block/parents") {
		ent := b.graph.Entities[EntityId(blockId.(int64))]
		ret = append(ret, ent.Block())
	}

	return ret
}

func (b Block) OutgoingRefs() []*Block {
	var ret []*Block

	for _, blockId := range b.Attr("block/refs") {
		ent := b.graph.Entities[EntityId(blockId.(int64))]
		ret = append(ret, ent.Block())
	}

	return ret
}

func (b Block) IncomingRefs() []*Block {
	var ret []*Block

	for _, block := range b.incomingRefs {
		ret = append(ret, block.Block())
	}

	return ret
}

func (b Block) Children() []*Block {
	var ret []*Block

	for _, block := range b.children {
		ret = append(ret, block.Block())
	}

	return ret
}

type Page struct {
	graph *RoamGraph

	blockId string
}

func (p Page) Block() *Block {
	return p.graph.Blocks[p.blockId]
}

type RoamGraph struct {
	Schema    map[edn.Keyword]RoamSchema `edn:"schema"`
	RawDatoms [][]any                    `edn:"datoms"`
	Entities  map[EntityId]*Entity
	Blocks    map[string]*Block
	Pages     map[string]*Page
}

func ParseGraph(r io.Reader) (*RoamGraph, error) {
	var _graph RoamGraph

	content, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	contentS := strings.TrimPrefix(string(content), "#datascript/DB ")

	err = edn.UnmarshalString(contentS, &_graph)
	if err != nil {
		return nil, err
	}

	graph := &_graph

	graph.Entities = make(map[EntityId]*Entity)
	graph.Blocks = make(map[string]*Block)
	graph.Pages = make(map[string]*Page)

	deferredRefs := make(map[EntityId][]*Entity)
	deferredChildren := make(map[EntityId][]*Entity)

	for _, datom := range graph.RawDatoms {
		if len(datom) != 4 {
			return nil, fmt.Errorf("can't parse datom %+v", datom)
		}

		entityId := EntityId(datom[0].(int64))
		attribute := datom[1].(edn.Keyword)
		value := datom[2]
		transactionId := TransactionId(datom[3].(int64))

		if _, ok := graph.Entities[entityId]; !ok {
			graph.Entities[entityId] = &Entity{graph: graph, Id: entityId}
		}

		entity := graph.Entities[entityId]

		entity.Values = append(entity.Values, Value{
			Attribute:     attribute,
			Value:         value,
			TransactionId: transactionId,
		})

		if attribute == edn.Keyword("block/uid") {
			if _, ok := graph.Blocks[value.(string)]; !ok {
				graph.Blocks[value.(string)] = &Block{
					graph: graph,
					ent:   entity,
					Id:    value.(string),
				}
			}
			graph.Entities[entityId].blockId = value.(string)
		} else if attribute == edn.Keyword("node/title") {
			graph.Pages[value.(string)] = &Page{
				graph:   graph,
				blockId: graph.Entities[entityId].blockId,
			}
		} else if attribute == edn.Keyword("block/parents") {
			target := EntityId(value.(int64))

			if _, ok := deferredChildren[target]; !ok {
				deferredChildren[target] = []*Entity{}
			}
			deferredChildren[target] = append(deferredChildren[target], entity)
		} else if attribute == edn.Keyword("block/refs") {
			target := EntityId(value.(int64))

			if _, ok := deferredRefs[target]; !ok {
				deferredRefs[target] = []*Entity{}
			}
			deferredRefs[target] = append(deferredRefs[target], entity)
		}
	}

	for target, incomingRefs := range deferredRefs {
		entity := graph.Entities[target]

		block := graph.Blocks[entity.blockId]

		block.incomingRefs = append(block.incomingRefs, incomingRefs...)
	}

	for target, children := range deferredChildren {
		entity := graph.Entities[target]

		block := graph.Blocks[entity.blockId]

		block.children = append(block.children, children...)
	}

	return graph, nil
}

type documentNode struct {
	block *Block

	id       string
	order    int
	text     string
	children []*documentNode
}

func sortChildren(children []*documentNode) []*documentNode {
	sort.Slice(children, func(i, j int) bool {
		return children[i].order < children[j].order
	})

	return children
}

func processText(text string, prefix string) string {
	depth := 0
	out := ""
	for _, c := range text {
		if c == '[' {
			depth += 1
			if depth == 2 {
				out += "_"
			}
		} else if c == ']' {
			depth -= 1
			if depth == 0 {
				out += "_"
			}
		} else if c == '\n' {
			out += "\n" + prefix
		} else {
			out += string(c)
		}
	}

	return out
}

func renderMarkdown(node *documentNode, prefix string) string {
	ret := ""

	text := processText(node.text, prefix)

	ret += fmt.Sprintf("%s- %s\n", prefix, text)

	node.children = sortChildren(node.children)

	for _, child := range node.children {
		ret += renderMarkdown(child, prefix+"  ")
	}

	return ret
}

func main() {
	flag.Parse()

	f, err := os.Open(*inputFile)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	graph, err := ParseGraph(f)
	if err != nil {
		log.Fatal(err)
	}

	publishPage := graph.Pages[*publishTag].Block()

	publishPrefix := fmt.Sprintf("#%s ", *publishTag)

	for _, ref := range publishPage.IncomingRefs() {
		uid := ref.Uid()
		text := ref.Text()

		log.Printf("%s %s", uid, text)

		if !strings.HasPrefix(text, publishPrefix) {
			continue
		}

		text = strings.TrimPrefix(text, publishPrefix)

		nodeList := make(map[string]*documentNode)

		root := &documentNode{
			block: ref,
			id:    uid,
			text:  text,
		}

		nodeList[uid] = root

		for _, child := range ref.Children() {
			nodeList[child.Uid()] = &documentNode{
				block: child,
				id:    child.Uid(),
				order: int(child.Order()),
				text:  child.Text(),
			}
		}

		for _, node := range nodeList {
			if node == root {
				continue
			}

			parents := node.block.Parents()
			directParent := parents[len(parents)-1]

			parentNode := nodeList[directParent.Uid()]
			parentNode.children = append(parentNode.children, node)
		}

		markdown := ""

		markdown += fmt.Sprintf("# %s\n\n", processText(root.text, ""))

		sortChildren(root.children)

		for _, child := range root.children {
			markdown += renderMarkdown(child, "")
		}

		err := os.WriteFile(path.Join("output", fmt.Sprintf("post_%s.md", root.id)), []byte(markdown), os.ModePerm)
		if err != nil {
			log.Fatalf("failed to write file: %v", err)
		}
	}
}
