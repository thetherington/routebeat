package beater

type QueryTerminals struct {
	Terminals Terminals `graphql:"terminals(input: {filters: [{id: \"isTlr\", booleanValue: true}, {id: \"isDst\", booleanValue: true}, {id:\"isSub\", booleanValue: true}, {id: \"tags\", listIncludes: [$tag]}]})"`
}

type SubscriptionTerminalsUpdated struct {
	TerminalsUpdated []Edge `graphql:"terminalsUpdated(input: {filters: [{id: \"isTlr\", booleanValue: true}, {id: \"isDst\", booleanValue: true}, {id:\"isSub\", booleanValue: true}, {id: \"tags\", listIncludes: [$tag]}]})"`
}

type Terminals struct {
	TotalCount int
	Edges      []Edge `graphql:"edges(limit: $limit)"`
}

type Edge struct {
	Id                        string
	Name                      string
	Tags                      []string
	IsSub                     bool
	IsDst                     bool
	Type                      string
	NamesetNames              []NamesetName
	RouteableTerminalFragment `graphql:"... on RouteableTerminal"`
}

type NamesetName struct {
	Id      string
	Name    string
	Nameset Nameset
}

type Nameset struct {
	Id   string
	Name string
}

type RouteableTerminalFragment struct {
	RoutedPhysicalSource *RoutedPhysicalSource
	SubscribedSource     *SubscribedSource
}

type RoutedPhysicalSource struct {
	Id           string
	Name         string
	IsSrc        bool
	NamesetNames []NamesetName
}

type SubscribedSource struct {
	Id           string
	Name         string
	IsSub        bool
	Tags         []string
	NamesetNames []NamesetName
}

/********** query template
query ($limit: Int!, $tag: String!) {
  terminals(
    input: {
      filters: [
        { id: "isTlr", booleanValue: true }
        { id: "isDst", booleanValue: true }
        { id: "isSub", booleanValue: true }
        { id: "tags", listIncludes: [$tag] }
      ]
    }
  ) {
    totalCount
    edges(limit: $limit) {
      id
      name
      tags
      isSub
      isDst
      type
      namesetNames {
        id
        name
        nameset {
          id
          name
        }
      }
      ... on RouteableTerminal {
        routedPhysicalSource {
          id
          name
          isSrc
          namesetNames {
            id
            name
            nameset {
              id
              name
            }
          }
        }
        subscribedSource {
          id
          name
          isSub
          tags
          namesetNames {
            id
            name
            nameset {
              id
              name
            }
          }
        }
      }
    }
  }
}
**********/

/********** subscription template
subscription ($tag: String!) {
  terminalsUpdated(
    input: {
      filters: [
        { id: "isTlr", booleanValue: true }
        { id: "isDst", booleanValue: true }
        { id: "isSub", booleanValue: true }
        { id: "tags", listIncludes: [$tag] }
      ]
    }
  ) {
    id
    name
    tags
    isSub
    isDst
    type
    namesetNames {
      id
      name
      nameset {
        id
        name
      }
    }
    ... on RouteableTerminal {
      routedPhysicalSource {
        id
        name
        isSrc
        namesetNames {
          id
          name
          nameset {
            id
            name
          }
        }
      }
      subscribedSource {
        id
        name
        isSub
        tags
        namesetNames {
          id
          name
          nameset {
            id
            name
          }
        }
      }
    }
  }
}
*/
