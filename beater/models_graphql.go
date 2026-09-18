package beater

type QueryTerminals struct {
	Terminals Terminals `graphql:"terminals(input: {filters: [{id: \"isTlr\", booleanValue: true}, {id: \"isDst\", booleanValue: true}, {id:\"isSub\", booleanValue: true}, {id: \"tags\", listIncludes: [$tag]}]})"`
}

type SubscriptionTerminalsUpdated struct {
	TerminalsUpdated []Edge `graphql:"terminalsUpdated(input: {filters: [{id: \"isTlr\", booleanValue: true}, {id: \"isDst\", booleanValue: true}, {id:\"isSub\", booleanValue: $isSub}, {id: \"tags\", listIncludes: [$tag]}]})"`
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
	Port                      *Port
	NamesetNames              []NamesetName
	RouteableTerminalFragment `graphql:"... on RouteableTerminal"`
}

type Port struct {
	Id        string
	Name      string
	Device    Device
	Addresses []Addresses
}

type Device struct {
	Id   string
	Name string
}

type Addresses struct {
	Id           string
	Name         string
	Backup       bool
	StreamType   string
	EthernetPort *EthernetPort
}

type EthernetPort struct {
	Id string
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
	Tags         []string
	NamesetNames []NamesetName
	Port         *Port
}

type SubscribedSource struct {
	Id           string
	Name         string
	IsSub        bool
	Tags         []string
	NamesetNames []NamesetName
}

/********** subscription template
subscription ($isSub: Boolean!, $tag: String!) {
  terminalsUpdated(
    input: {
      filters: [
        { id: "isTlr", booleanValue: true }
        { id: "isDst", booleanValue: true }
        { id: "isSub", booleanValue: $isSub }
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
    port {
      id
      name
      device {
        id
        name
      }
      addresses {
        id
        name
        backup
        streamType
        ethernetPort {
          id
        }
      }
    }
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
        tags
        namesetNames {
          id
          name
          nameset {
            id
            name
          }
        }
        port {
          id
          name
          device {
            id
            name
          }
          addresses {
            id
            name
            backup
            streamType
            ethernetPort {
              id
            }
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

**********/
