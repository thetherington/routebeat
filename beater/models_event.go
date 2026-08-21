package beater

type SchedulerAPGEvent struct {
	EndDate       string `json:"end_date"`
	OutputType    string `json:"output_type"`
	OutputID      string `json:"output_id"`
	EventID       string `json:"event_id"`
	Error         string `json:"error"`
	History       []any  `json:"history"`
	Start         int64  `json:"start"`
	StartDate     string `json:"start_date"`
	OutputPooled  bool   `json:"output_pooled"`
	ScheduleID    string `json:"schedule_id"`
	Entry         string `json:"entry"`
	State         int    `json:"state"`
	InputBlocking bool   `json:"input_blocking"`
	InputPooled   bool   `json:"input_pooled"`
	Task          string `json:"task"`
	InputID       string `json:"input_id"`
	EndOffset     string `json:"end_offset"`
	InputType     string `json:"input_type"`
	Input         string `json:"input"`
	EventParams   struct {
		ApgID    string `json:"apg_id"`
		PriSrc   string `json:"pri_src"`
		SecSrc   string `json:"sec_src"`
		Historic bool   `json:"historic"`
		BusName  string `json:"bus_name"`
		ApgRule  string `json:"apg_rule"`
	} `json:"event_params"`
	Recur struct {
	} `json:"recur"`
	OutputBlocking bool     `json:"output_blocking"`
	Comment        string   `json:"comment"`
	Tags           []string `json:"tags"`
	End            int64    `json:"end"`
	StartOffset    string   `json:"start_offset"`
	Output         string   `json:"output"`
}

/**
{
        "end_date": "2026/08/16 14:00:00",
        "output_type": "magnum",
        "output_id": "46e42e0d-c90b-55e0-a3b6-c4eff685516e",
        "event_id": "88a3dccc-0725-4e15-a52f-8694e6675abe",
        "error": "",
        "history": [],
        "start": 1786885320000000000,
        "start_date": "2026/08/16 13:02:00",
        "output_pooled": false,
        "schedule_id": "12856216&1786885320000000000",
        "entry": "manual",
        "state": 1,
        "input_blocking": false,
        "input_pooled": false,
        "task": "apg",
        "input_id": "c8348ef8-f852-537f-90fa-60e75da8dee3",
        "end_offset": "0",
        "input_type": "magnum",
        "input": "SILENCE",
        "event_params": {
          "apg_id": "36857574",
          "pri_src": "{event.input}",
          "sec_src": "SILENCE",
          "historic": true,
          "bus_name": "{event.output}",
          "apg_rule": "NOOP"
        },
        "recur": {},
        "output_blocking": true,
        "comment": "",
        "tags": [
          "MAIN"
        ],
        "end": 1786888800000000000,
        "start_offset": "0",
        "output": "TLUPDT"
      }
	**/
