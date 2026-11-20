from conftest import FakeLLM, ai, run_request, stream_run, tool, types


async def test_direct_answer_streams_the_answer_then_done_with_usage(make_client):
    llm = FakeLLM(script=[ai("The total is 35.", inp=500, out=20, cache_read=300, cache_write=100)])
    async with make_client(llm) as client:
        status, events = await stream_run(client, run_request())
    assert status == 200
    assert types(events) == ["answer_delta", "done"]
    assert events[0]["text"] == "The total is 35."
    done = events[1]
    assert done["answer"] == "The total is 35."
    assert done["status"] == "done"
    # input_tokens excludes cache reads and writes, as the contract says.
    assert done["usage"] == {"provider": "anthropic", "model": "claude-haiku-5-5", "input_tokens": 100,
                             "output_tokens": 20, "cached_tokens": 300, "cache_write_tokens": 100,
                             "steps": 0}


async def test_tool_loop_streams_each_step_and_feeds_results_back(make_client, go):
    go.respond = lambda name, body: {"output": "sum\n35\n(1 rows)", "chart_ids": ["c9"]}
    llm = FakeLLM(script=[ai(tool_calls=[tool("run_sql", sql="select sum(amount) from sales")]),
                          ai("Total is 35.")])
    async with make_client(llm) as client:
        _, events = await stream_run(client, run_request())
    assert types(events) == ["step_start", "tool_call", "tool_result", "answer_delta", "done"]
    assert events[1] == {"type": "tool_call", "step": 1, "tool": "run_sql",
                         "input": {"sql": "select sum(amount) from sales"}}
    assert events[2]["chart_ids"] == ["c9"] and events[2]["error"] == ""
    assert go.requests == [{"tool": "run_sql", "run_id": "run-1", "session_id": "sess-1",
                            "input": {"sql": "select sum(amount) from sales"}}]
    # The second model call sees the tool's output.
    assert "35" in llm.calls[1]["messages"][-1].content
    assert events[-1]["usage"]["steps"] == 1
    assert events[-1]["usage"]["input_tokens"] == 200  # summed over both model calls


async def test_tool_errors_reach_the_model_so_it_can_retry(make_client, go):
    replies = iter([{"output": "", "error": "Binder Error: column \"amout\" not found", "chart_ids": []},
                    {"output": "35", "chart_ids": []}])
    go.respond = lambda name, body: next(replies)
    llm = FakeLLM(script=[ai(tool_calls=[tool("run_sql", sql="select sum(amout) from sales")]),
                          ai(tool_calls=[tool("run_sql", sql="select sum(amount) from sales")]),
                          ai("35")])
    async with make_client(llm) as client:
        _, events = await stream_run(client, run_request())
    assert "Binder Error" in llm.calls[1]["messages"][-1].content
    assert next(e["error"] for e in events if e["type"] == "tool_result").startswith("Binder Error")
    assert events[-1]["status"] == "done" and events[-1]["usage"]["steps"] == 2


async def test_step_budget_stops_tools_and_forces_a_best_effort_answer(make_client, go):
    llm = FakeLLM(script=[ai(tool_calls=[tool("run_python", code="1")]),
                          ai(tool_calls=[tool("run_python", code="2"), tool("run_python", code="3")]),
                          ai("Partial answer: hit the limit.")])
    async with make_client(llm) as client:
        _, events = await stream_run(client, run_request(limits={"max_steps": 2, "max_tokens": 1024}))
    assert len(go.requests) == 2  # the third call was never run
    done = events[-1]
    assert done["status"] == "budget_exhausted" and done["answer"] == "Partial answer: hit the limit."
    last_call = llm.calls[-1]
    assert last_call["tool_choice"] == "none"
    assert "tool calls allowed" in last_call["messages"][-1].content


async def test_runs_need_the_internal_token(make_client):
    async with make_client(FakeLLM(script=[])) as client:
        status, _ = await stream_run(client, run_request(), token="wrong")
    assert status == 401


async def test_model_failure_becomes_an_error_event(make_client):
    llm = FakeLLM(script=[ai(tool_calls=[tool("describe_dataset")]), RuntimeError("overloaded")])
    async with make_client(llm) as client:
        _, events = await stream_run(client, run_request())
    assert events[-1]["type"] == "error"
    assert "overloaded" in events[-1]["error"]
    assert events[-1]["usage"]["input_tokens"] == 100  # spend so far is still reported


async def test_unknown_provider_is_rejected_before_streaming(make_client):
    async with make_client(FakeLLM(script=[])) as client:
        status, _ = await stream_run(client, run_request(model={"provider": "openai", "name": "x"}))
    assert status == 422


async def test_prompt_has_schema_and_history_and_caches_for_anthropic(make_client):
    llm = FakeLLM(script=[ai("ok")])
    history = [{"question": "total?", "answer": "35",
                "steps": [{"tool": "run_sql", "input": {"sql": "select 1"}, "output": "35"}]}]
    schema = [{"name": "Sales Data", "rows": 3, "columns": [{"name": "sold on", "type": "DATE"}],
               "sample_rows": [["2024-01-05"]]}]
    async with make_client(llm) as client:
        await stream_run(client, run_request(history=history, schema=schema, question="by region?"))
    system, past_q, past_a, question = llm.calls[0]["messages"]
    block = system.content[0]
    assert block["cache_control"] == {"type": "ephemeral"}
    assert '"Sales Data" (3 rows)' in block["text"] and '"sold on" DATE' in block["text"]
    assert past_q.content == "total?"
    assert "select 1" in past_a.content and past_a.content.endswith("35")
    assert question.content == "by region?"


async def test_deepseek_gets_a_plain_system_prompt(make_client):
    llm = FakeLLM(script=[ai("ok")])
    async with make_client(llm) as client:
        await stream_run(client, run_request(model={"provider": "deepseek", "name": "deepseek-flash"}))
    assert isinstance(llm.calls[0]["messages"][0].content, str)
