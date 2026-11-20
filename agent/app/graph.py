"""The LangGraph agent: a ReAct loop with a tool-call budget.

    START → agent ─(tool calls)→ tools ─(budget left)→ agent
              │                    └──(budget used)→ wrap_up → END
              └─(final answer)→ END

Nodes report progress through LangGraph's custom stream, one event per
dict, using the event shapes of the internal contract.
"""

from __future__ import annotations

from typing import Annotated, TypedDict

from langchain_core.language_models.chat_models import BaseChatModel
from langchain_core.messages import AIMessage, BaseMessage, HumanMessage, ToolMessage
from langgraph.config import get_stream_writer
from langgraph.graph import END, START, StateGraph
from langgraph.graph.message import add_messages

from app.prompts import WRAP_UP
from app.schemas import Usage
from app.tools import TOOLS, ToolClient


class RunState(TypedDict):
    messages: Annotated[list[BaseMessage], add_messages]
    steps: int
    answer: str
    status: str


def text_of(message: AIMessage) -> str:
    """Anthropic returns a list of content blocks; DeepSeek returns a string."""
    if isinstance(message.content, str):
        return message.content
    return "".join(b.get("text", "") for b in message.content if isinstance(b, dict) and b.get("type") == "text")


def build_graph(llm: BaseChatModel, tools: ToolClient, usage: Usage, max_steps: int):
    with_tools = llm.bind_tools(TOOLS)
    # The history holds tool calls, so providers need the tool definitions even
    # on the last call; tool_choice="none" stops further calls.
    no_more_tools = llm.bind_tools(TOOLS, tool_choice="none")

    def finish(message: AIMessage, status: str) -> dict:
        answer = text_of(message).strip()
        get_stream_writer()({"type": "answer_delta", "text": answer})
        return {"messages": [message], "answer": answer, "status": status}

    async def agent(state: RunState) -> dict:
        reply = await with_tools.ainvoke(state["messages"])
        usage.add(reply.usage_metadata)
        if reply.tool_calls:
            return {"messages": [reply]}
        return finish(reply, "done")

    async def run_tools(state: RunState) -> dict:
        write = get_stream_writer()
        last = state["messages"][-1]
        steps = state["steps"]
        results: list[ToolMessage] = []
        for call in last.tool_calls:
            if steps >= max_steps:
                results.append(ToolMessage("Not run: the tool-call limit was reached.", tool_call_id=call["id"]))
                continue
            steps += 1
            write({"type": "step_start", "step": steps})
            write({"type": "tool_call", "step": steps, "tool": call["name"], "input": call["args"]})
            result = await tools.call(call["name"], call["args"])
            write({"type": "tool_result", "step": steps, "tool": call["name"], "summary": result.summary(),
                   "error": result.error, "chart_ids": result.chart_ids})
            results.append(ToolMessage(result.for_model(), tool_call_id=call["id"]))
        usage.steps = steps
        return {"messages": results, "steps": steps}

    async def wrap_up(state: RunState) -> dict:
        nudge = HumanMessage(WRAP_UP.format(max_steps=max_steps))
        reply = await no_more_tools.ainvoke([*state["messages"], nudge])
        usage.add(reply.usage_metadata)
        return finish(reply, "budget_exhausted") | {"messages": [nudge, reply]}

    def after_agent(state: RunState) -> str:
        return "tools" if state["messages"][-1].tool_calls and not state.get("status") else END

    def after_tools(state: RunState) -> str:
        return "wrap_up" if state["steps"] >= max_steps else "agent"

    g = StateGraph(RunState)
    g.add_node("agent", agent)
    g.add_node("tools", run_tools)
    g.add_node("wrap_up", wrap_up)
    g.add_edge(START, "agent")
    g.add_conditional_edges("agent", after_agent, ["tools", END])
    g.add_conditional_edges("tools", after_tools, ["agent", "wrap_up"])
    g.add_edge("wrap_up", END)
    return g.compile()
