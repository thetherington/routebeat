package beater

type QueryTerminals struct {
	Terminals Terminals `graphql:"terminals(input: {filters: [{id: \"isDst\", booleanValue: true}, {id:\"isSub\", booleanValue: true}, {id: \"tags\", value: $tag}]})"`
}

type SubscriptionTerminalsUpdated struct {
	TerminalsUpdated []Edge `graphql:"terminalsUpdated(input: {filters: [{id: \"isDst\", booleanValue: true}, {id:\"isSub\", booleanValue: true}, {id: \"tags\", value: $tag}]})"`
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
	Tags         []string
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
query {
	terminals(input: {filters: [{id: "isDst", booleanValue: true}, {id:"isSub", booleanValue: true}, {id: "tags", value: "IPAN"}]}){
    	totalCount
    	edges(limit: 2000) {
			id name tags isSub isDst type
			... on RouteableTerminal {
				routedPhysicalSource {
					id name isSrc
					namesetNames {
						id name
						nameset{
							id name
						}
					}
				}
				subscribedSource {
					id name isSub
					namesetNames {
						id name
						nameset{
							id name
						}
					}
				}
			}
			namesetNames {
				id name
				nameset {
					id name
				}
			}
		}
  	}
}
**********/
