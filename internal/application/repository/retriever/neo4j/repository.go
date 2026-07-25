package neo4j

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/custom/modules/chatretrieval"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// Neo4jRepository is a repository for Neo4j
type Neo4jRepository struct {
	driver      neo4j.Driver
	nodePrefix  string
	schemaMu    sync.Mutex
	schemaReady bool
}

const (
	graphEntityBaseLabel          = "ENTITY"
	graphEntityIdentityConstraint = "weknora_graph_entity_identity"
)

// NewNeo4jRepository creates a new Neo4j repository
func NewNeo4jRepository(driver neo4j.Driver) interfaces.RetrieveGraphRepository {
	return &Neo4jRepository{driver: driver, nodePrefix: "ENTITY"}
}

// _remove_hyphen removes hyphens from a string
func _remove_hyphen(s string) string {
	return strings.ReplaceAll(s, "-", "_")
}

// Labels returns the labels for a namespace
func (n *Neo4jRepository) Labels(namespace types.NameSpace) []string {
	// Every graph node carries a stable base label in addition to the
	// knowledge-base/document labels. The base label is what allows Neo4j to
	// enforce one (knowledge, name) node identity even when graph batches from
	// several workers arrive concurrently.
	res := []string{graphEntityBaseLabel}
	for _, label := range namespace.Labels() {
		res = append(res, n.nodePrefix+_remove_hyphen(label))
	}
	return res
}

// Label returns the label for a namespace
func (n *Neo4jRepository) Label(namespace types.NameSpace) string {
	labels := n.Labels(namespace)
	// Read/delete queries intentionally match the namespace labels without
	// requiring the new base label. That keeps reparsing able to remove graph
	// nodes written before the identity constraint was introduced; all newly
	// written nodes still receive the base label through Labels.
	if len(labels) > 0 && labels[0] == graphEntityBaseLabel {
		labels = labels[1:]
	}
	return strings.Join(labels, ":")
}

// AddGraph adds a graph to the Neo4j repository
func (n *Neo4jRepository) AddGraph(ctx context.Context, namespace types.NameSpace, graphs []*types.GraphData) error {
	if n.driver == nil {
		logger.Warnf(ctx, "NOT SUPPORT RETRIEVE GRAPH")
		return nil
	}
	if err := n.ensureGraphSchema(ctx); err != nil {
		return err
	}
	for _, graph := range graphs {
		if graph == nil {
			continue
		}
		if err := n.addGraph(ctx, namespace, graph); err != nil {
			return err
		}
	}
	return nil
}

// ensureGraphSchema installs the shared entity identity constraint once per
// process. CREATE ... IF NOT EXISTS is safe when several application replicas
// initialize at the same time. We intentionally only mark the schema ready
// after success so a temporary Neo4j outage is retried by the next task.
func (n *Neo4jRepository) ensureGraphSchema(ctx context.Context) error {
	n.schemaMu.Lock()
	defer n.schemaMu.Unlock()
	if n.schemaReady {
		return nil
	}

	session := n.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)
	query := fmt.Sprintf(
		"CREATE CONSTRAINT %s IF NOT EXISTS FOR (n:%s) REQUIRE (n.kg, n.name) IS UNIQUE",
		graphEntityIdentityConstraint,
		graphEntityBaseLabel,
	)
	result, err := session.Run(ctx, query, nil)
	if err != nil {
		return fmt.Errorf("ensure graph entity identity constraint: %w", err)
	}
	if _, err := result.Consume(ctx); err != nil {
		return fmt.Errorf("commit graph entity identity constraint: %w", err)
	}
	n.schemaReady = true
	return nil
}

// addGraph adds a graph to the Neo4j repository
func (n *Neo4jRepository) addGraph(ctx context.Context, namespace types.NameSpace, graph *types.GraphData) error {
	session := n.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)

	_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
		// Node import query
		node_import_query := `
			UNWIND $data AS row
			CALL apoc.merge.node(row.labels, {name: row.name, kg: row.knowledge_id}, row.props, {}) YIELD node
			SET node.chunks = apoc.coll.union(node.chunks, row.chunks)
			RETURN distinct 'done' AS result
		`
		nodeData := make([]map[string]interface{}, 0, len(graph.Node))
		for _, node := range graph.Node {
			if node == nil || strings.TrimSpace(node.Name) == "" {
				continue
			}
			nodeData = append(nodeData, map[string]interface{}{
				"name":         strings.TrimSpace(node.Name),
				"knowledge_id": namespace.Knowledge,
				"props":        map[string][]string{"attributes": node.Attributes},
				"chunks":       node.Chunks,
				"labels":       n.Labels(namespace),
			})
		}
		if len(nodeData) > 0 {
			if _, err := tx.Run(ctx, node_import_query, map[string]interface{}{"data": nodeData}); err != nil {
				return nil, fmt.Errorf("failed to create nodes: %v", err)
			}
		}

		// Relationship import query
		rel_import_query := `
			UNWIND $data AS row
			CALL apoc.merge.node(row.source_labels, {name: row.source, kg: row.knowledge_id}, {}, {}) YIELD node as source
			CALL apoc.merge.node(row.target_labels, {name: row.target, kg: row.knowledge_id}, {}, {}) YIELD node as target
			CALL apoc.merge.relationship(source, row.type, {}, row.attributes, target) YIELD rel
			RETURN distinct 'done'
		`
		relData := make([]map[string]interface{}, 0, len(graph.Relation))
		for _, rel := range graph.Relation {
			if rel == nil ||
				strings.TrimSpace(rel.Node1) == "" ||
				strings.TrimSpace(rel.Node2) == "" ||
				strings.TrimSpace(rel.Type) == "" {
				continue
			}
			relData = append(relData, map[string]interface{}{
				"source":        strings.TrimSpace(rel.Node1),
				"target":        strings.TrimSpace(rel.Node2),
				"knowledge_id":  namespace.Knowledge,
				"type":          strings.TrimSpace(rel.Type),
				"source_labels": n.Labels(namespace),
				"target_labels": n.Labels(namespace),
			})
		}
		if len(relData) > 0 {
			if _, err := tx.Run(ctx, rel_import_query, map[string]interface{}{"data": relData}); err != nil {
				return nil, fmt.Errorf("failed to create relationships: %v", err)
			}
		}
		return nil, nil
	})
	if err != nil {
		logger.Errorf(ctx, "failed to add graph: %v", err)
		return err
	}
	return nil
}

// DelGraph deletes a graph from the Neo4j repository
func (n *Neo4jRepository) DelGraph(ctx context.Context, namespaces []types.NameSpace) error {
	if n.driver == nil {
		logger.Warnf(ctx, "NOT SUPPORT RETRIEVE GRAPH")
		return nil
	}
	session := n.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)

	result, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
		for _, namespace := range namespaces {
			labelExpr := n.Label(namespace)

			deleteRelsQuery := `
				CALL apoc.periodic.iterate(
					"MATCH (n:` + labelExpr + ` {kg: $knowledge_id})-[r]-(m:` + labelExpr + ` {kg: $knowledge_id}) RETURN r",
					"DELETE r",
					{batchSize: 1000, parallel: true, params: {knowledge_id: $knowledge_id}}
				) YIELD batches, total
				RETURN total
        	`
			if _, err := tx.Run(ctx, deleteRelsQuery, map[string]interface{}{"knowledge_id": namespace.Knowledge}); err != nil {
				return nil, fmt.Errorf("failed to delete relationships: %v", err)
			}

			deleteNodesQuery := `
				CALL apoc.periodic.iterate(
					"MATCH (n:` + labelExpr + ` {kg: $knowledge_id}) RETURN n",
					"DELETE n",
					{batchSize: 1000, parallel: true, params: {knowledge_id: $knowledge_id}}
				) YIELD batches, total
				RETURN total
        	`
			if _, err := tx.Run(ctx, deleteNodesQuery, map[string]interface{}{"knowledge_id": namespace.Knowledge}); err != nil {
				return nil, fmt.Errorf("failed to delete nodes: %v", err)
			}
		}
		return nil, nil
	})
	if err != nil {
		return err
	}
	logger.Infof(ctx, "delete graph result: %v", result)
	return nil
}

// SearchNode searches for nodes in the Neo4j repository
func (n *Neo4jRepository) SearchNode(
	ctx context.Context,
	namespace types.NameSpace,
	nodes []string,
) (*types.GraphData, error) {
	if n.driver == nil {
		logger.Warnf(ctx, "NOT SUPPORT RETRIEVE GRAPH")
		return nil, nil
	}
	session := n.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)

	result, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
		labelExpr := n.Label(namespace)
		query := `
			MATCH (n:` + labelExpr + `)
			WITH n, [
				nodeText IN $nodes
				WHERE size(trim(nodeText)) >= 2
					AND toLower(n.name) CONTAINS toLower(trim(nodeText))
			] AS matched_terms
			WHERE size(matched_terms) > 0
			WITH n,
				CASE
					WHEN any(nodeText IN matched_terms
						WHERE toLower(n.name) = toLower(trim(nodeText)))
					THEN 1 ELSE 0
				END AS exact_match,
				size(matched_terms) AS matched_count,
				reduce(longest = 0, nodeText IN matched_terms |
					CASE
						WHEN size(trim(nodeText)) > longest
						THEN size(trim(nodeText))
						ELSE longest
					END
				) AS longest_term
			ORDER BY exact_match DESC, matched_count DESC, longest_term DESC,
				size(n.name) ASC, n.name ASC
			LIMIT $anchor_limit
			MATCH (n)-[r]-(m:` + labelExpr + `)
			RETURN n, r, m, exact_match, matched_count, longest_term
			ORDER BY exact_match DESC, matched_count DESC, longest_term DESC,
				n.name ASC, m.name ASC
			LIMIT $relation_limit
		`
		params := map[string]interface{}{
			"nodes":          nodes,
			"anchor_limit":   chatretrieval.GraphAnchorLimit,
			"relation_limit": chatretrieval.GraphRelationLimit,
		}
		result, err := tx.Run(ctx, query, params)
		if err != nil {
			return nil, fmt.Errorf("failed to run query: %v", err)
		}

		graphData := &types.GraphData{}
		nodeSeen := make(map[string]bool)
		for result.Next(ctx) {
			record := result.Record()
			node, _ := record.Get("n")
			rel, _ := record.Get("r")
			targetNode, _ := record.Get("m")

			nodeData := node.(neo4j.Node)
			targetNodeData := targetNode.(neo4j.Node)

			// Convert node to types.Node
			for _, n := range []neo4j.Node{nodeData, targetNodeData} {
				nameStr := n.Props["name"].(string)
				if _, ok := nodeSeen[nameStr]; !ok {
					nodeSeen[nameStr] = true
					graphData.Node = append(graphData.Node, &types.GraphNode{
						Name:       nameStr,
						Chunks:     listI2listS(n.Props["chunks"].([]interface{})),
						Attributes: listI2listS(n.Props["attributes"].([]interface{})),
					})
				}
			}

			// Convert relationship to types.Relation
			relData := rel.(neo4j.Relationship)
			graphData.Relation = append(graphData.Relation, &types.GraphRelation{
				Node1: nodeData.Props["name"].(string),
				Node2: targetNodeData.Props["name"].(string),
				Type:  relData.Type,
			})
		}
		return graphData, nil
	})
	if err != nil {
		logger.Errorf(ctx, "search node failed: %v", err)
		return nil, err
	}
	return result.(*types.GraphData), nil
}

func listI2listS(list []any) []string {
	result := make([]string, len(list))
	for i, v := range list {
		result[i] = fmt.Sprintf("%v", v)
	}
	return result
}
