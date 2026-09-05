import {
  createElement,
  useEffect,
  useRef,
  useState,
  type FC,
  type ReactNode,
} from "react";
import { connect, onRender, sendTeaEvent, type WireNode } from "../socket";

/** Props every Tea-rendered component receives. */
export interface TeaComponentProps {
  node: WireNode;
  /** emit fires one of the node's Go handlers by event name. */
  emit: (event: string, payload?: unknown) => void;
  children?: ReactNode;
}

export type ComponentRegistry = Record<string, FC<TeaComponentProps>>;

// extraComponents lets createApp add explicitly-registered components on
// top of the paired components/ folders.
let appRegistry: ComponentRegistry = {};

export function setRegistry(reg: ComponentRegistry): void {
  appRegistry = reg;
}

function emitFor(node: WireNode) {
  return (event: string, payload?: unknown) => {
    const id = node.handlers?.[event];
    if (id) sendTeaEvent(id, payload);
  };
}

function styleHints(props: Record<string, unknown> | undefined): {
  style: Record<string, string | number>;
  className: string;
} {
  const style: Record<string, string | number> = {};
  let className = "";
  if (props) {
    if (typeof props.gap === "number") style.gap = props.gap;
    if (typeof props.pad === "number") style.padding = props.pad;
    if (props.grow === true) style.flexGrow = 1;
    if (typeof props.class === "string") className = props.class;
  }
  return { style, className };
}

function renderChildren(children: WireNode[] | undefined): ReactNode[] {
  return (children ?? []).map((c, i) => <NodeView key={c.key ?? i} node={c} />);
}

// TeaInput is semi-controlled: keystrokes echo locally at once and
// stream to Go; the Go value only overrides the field when it differs
// from what we last sent (a real remote change, not our own echo
// arriving late). A pure controlled input over a websocket round trip
// drops and jumbles fast typing.
function TeaInput({ node }: { node: WireNode }) {
  const server = String(node.props?.value ?? "");
  const [local, setLocal] = useState(server);
  const lastSent = useRef<string | null>(null);
  const emit = emitFor(node);

  useEffect(() => {
    if (lastSent.current === null || server !== lastSent.current) {
      setLocal(server);
      lastSent.current = null;
    } else if (server === lastSent.current) {
      lastSent.current = null;
    }
  }, [server]);

  return (
    <input
      className="gantry-tea-input"
      type="text"
      value={local}
      placeholder={typeof node.props?.placeholder === "string" ? node.props.placeholder : undefined}
      onChange={(e) => {
        setLocal(e.target.value);
        lastSent.current = e.target.value;
        emit("change", e.target.value);
      }}
    />
  );
}

function NodeView({ node }: { node: WireNode }) {
  const { style, className } = styleHints(node.props);
  const cls = (base: string) => base + (className ? " " + className : "");
  const emit = emitFor(node);

  switch (node.type) {
    case "column":
      return (
        <div className={cls("gantry-tea-column")} style={style}>
          {renderChildren(node.children)}
        </div>
      );
    case "row":
      return (
        <div className={cls("gantry-tea-row")} style={style}>
          {renderChildren(node.children)}
        </div>
      );
    case "text":
      return (
        <span className={cls("gantry-tea-text")} style={style}>
          {String(node.props?.text ?? "")}
        </span>
      );
    case "heading":
      return (
        <h2 className={cls("gantry-tea-heading")} style={style}>
          {String(node.props?.text ?? "")}
        </h2>
      );
    case "button":
      return (
        <button
          type="button"
          className={cls("gantry-tea-button")}
          style={style}
          onClick={() => emit("click")}
        >
          {String(node.props?.label ?? "")}
        </button>
      );
    case "input":
      return <TeaInput node={node} />;
    case "checkbox":
      return (
        <label className={cls("gantry-tea-checkbox")} style={style}>
          <input
            type="checkbox"
            checked={node.props?.checked === true}
            onChange={(e) => emit("change", e.target.checked)}
          />
          <span>{String(node.props?.label ?? "")}</span>
        </label>
      );
    case "select":
      return (
        <select
          className={cls("gantry-tea-select")}
          style={style}
          value={String(node.props?.value ?? "")}
          onChange={(e) => emit("change", e.target.value)}
        >
          {((node.props?.options as string[]) ?? []).map((o) => (
            <option key={o} value={o}>
              {o}
            </option>
          ))}
        </select>
      );
    case "divider":
      return <hr className="gantry-tea-divider" />;
    case "spacer":
      return <div className="gantry-tea-spacer" />;
    case "progress": {
      const v = Math.min(1, Math.max(0, Number(node.props?.value ?? 0)));
      return (
        <div className={cls("gantry-tea-progress")} style={style}>
          <div className="gantry-tea-progress-fill" style={{ width: (v * 100).toFixed(1) + "%" }} />
        </div>
      );
    }
    default: {
      const Custom = appRegistry[node.type];
      if (Custom) {
        return createElement(Custom, { node, emit }, renderChildren(node.children));
      }
      return <div className="gantry-tea-unknown">[unknown component: {node.type}]</div>;
    }
  }
}

/**
 * TeaView renders the Go-driven tree for the current page. Put one in
 * any page whose .go half has a Model; pages without a Model simply do
 * not use it.
 */
export function TeaView() {
  const [tree, setTree] = useState<WireNode | null>(null);
  useEffect(() => {
    connect();
    onRender(setTree);
    return () => onRender(null);
  }, []);
  if (!tree) return null;
  return <NodeView node={tree} />;
}
