//
//  NanoMusePromptDiet.swift
//  nanoMuse
//
//  The same instructions in fewer tokens. Every call carries the tool schemas again, and
//  the `browser_use` schema was the fattest of them: 7.9k characters, two thirds on cookies,
//  viewport and infinite scroll, which the model reached for in a handful of the week's 420
//  browser calls from iPhones. The actions, parameters, enum values and required list stay
//  upstream's; only the wording is ours, and every action is still named. Android:
//  PromptDiet (which also condenses the Android tools section of the prompt).
//

import Foundation

enum NanoMusePromptDiet {
    private static let browserUse =
        "Control a web browser with up to 3 tabs. Actions: navigate (web URLs, and minis:// resource URLs " +
        "such as minis://workspace/index.html to preview session files; their sub-resources resolve), " +
        "screenshot (returns an image; use it to see the page; full_page=true for the whole page), click and " +
        "type (by selector or coordinates), get_text and get_readable to extract content, scroll, " +
        "scroll_and_collect for infinite-scroll pages (collects unique items across scroll positions in one " +
        "call), find_elements, get_page_info, get_backbone (a simplified DOM tree), hover, execute_js, " +
        "fetch (downloads with the page's session; returns metadata and a minis:// URL), new_tab, close_tab, " +
        "list_tabs (tab_id targets a tab; default the most recently used), set_user_agent, set_viewport " +
        "(viewport_width and viewport_height for this session, e.g. before screenshotting a 1920×1080 " +
        "composition; reset=true drops the override), wait_for_dom_stable (after navigation or async " +
        "loading; default 10 s), get_cookies and set_cookies for the current site, HttpOnly included " +
        "(get_cookies returns a summary and an env file, never raw values: " +
        "`. /var/minis/offloads/env_cookies_xxx.sh && command`). " +
        "Do NOT use this tool for minis:// action URLs (open_terminal, views, settings): those are app deep " +
        "links, write them as Markdown links in chat."

    /// Shorter descriptions for the parameters whose upstream text runs to a paragraph.
    private static let browserParams: [String: String] = [
        "selector": "CSS selector of the element (click, type, get_text, scroll, hover, find_elements). " +
            "For scroll: the scrollable container; omitted, the best one is detected.",
        "script": "JavaScript for execute_js; runs inside an async function, so `await` and a top-level `return` work.",
        "scroll_count": "Scroll steps for scroll_and_collect (default 10, max 20), each by 'amount' pixels.",
        "item_selector": "CSS selector of one content item for scroll_and_collect (e.g. 'article'); omitted, " +
            "repeated elements are detected.",
        "tab_id": "Target tab (default the most recently used); list_tabs shows them.",
        "keywords": "get_cookies: filter by name, space-separated; fuzzy=true (default) needs every keyword in " +
            "the name, fuzzy=false an exact match with one of them. Omit for all cookies.",
        "fuzzy": "get_cookies: fuzzy (contains all) or exact (equals one) name matching; default true.",
        "cookies": "set_cookies: a JSON array of {name, value, domain?, path?, secure?, http_only?, expires? " +
            "(Unix seconds; omit for a session cookie)}. Field names from common exports (httpOnly, " +
            "expirationDate, sameSite) are accepted, so cookies from a browser extension or Playwright can be " +
            "passed as they are.",
        "timeout": "wait_for_dom_stable: seconds to wait (default 10).",
        "viewport_width": "set_viewport: width in CSS pixels (e.g. 1920); with viewport_height unless reset=true.",
        "viewport_height": "set_viewport: height in CSS pixels (e.g. 1080); with viewport_width unless reset=true.",
        "reset": "set_viewport: true clears this session's override.",
        "full_page": "screenshot: true captures the whole scrollable page (capped at 32768 px); default false, the viewport.",
    ]

    /// The tools as the model sees them: `browser_use` worded densely, everything else untouched.
    static func tools(_ tools: [AgentToolDefinition]) -> [AgentToolDefinition] {
        tools.map { tool in
            guard tool.name == "browser_use" else { return tool }
            var parameters: [String: AgentToolParam] = [:]
            for (key, param) in tool.parameters {
                if let shorter = browserParams[key] {
                    parameters[key] = AgentToolParam(type: param.type, description: shorter, enumValues: param.enumValues)
                } else {
                    parameters[key] = param
                }
            }
            return AgentToolDefinition(
                name: tool.name,
                description: browserUse,
                parameters: parameters,
                required: tool.required,
                propertyOrdering: tool.propertyOrdering
            )
        }
    }
}
