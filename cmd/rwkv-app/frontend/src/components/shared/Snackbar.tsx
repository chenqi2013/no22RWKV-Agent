import { useEffect, useState } from "react";
import { AlertCircle, CheckCircle2, Info, X } from "lucide-react";

interface Props {
  message: string;
  severity?: "info" | "success" | "error";
  duration?: number;
  onClose: () => void;
}

export default function Snackbar({ message, severity = "info", duration = 1000, onClose }: Props) {
  const [exiting, setExiting] = useState(false);

  useEffect(() => {
    const t = setTimeout(() => setExiting(true), duration);
    return () => clearTimeout(t);
  }, [duration]);

  useEffect(() => {
    if (exiting) {
      const t = setTimeout(onClose, 200);
      return () => clearTimeout(t);
    }
  }, [exiting, onClose]);

  const Icon = severity === "success" ? CheckCircle2 : severity === "error" ? AlertCircle : Info;
  const iconClass = severity === "success" ? "text-brand-bright" : severity === "error" ? "text-danger" : "text-white/80";

  return (
    <div
      className={`fixed bottom-5 left-1/2 z-[1100] flex -translate-x-1/2 items-center gap-2 rounded-lg bg-ink px-3 py-2 text-white shadow-[0_6px_20px_rgba(43,39,33,.28)] transition-all duration-200 ${exiting ? "translate-y-1 opacity-0" : "opacity-100"}`}
      role="status"
    >
      <Icon className={`flex-none ${iconClass}`} size={16} />
      <span className="text-xs leading-4">{message}</span>
      <button
        className="grid h-5 w-5 flex-none place-items-center rounded-full text-white/70 transition-colors hover:bg-white/10 hover:text-white"
        onClick={() => setExiting(true)}
        aria-label="关闭"
      >
        <X size={13} />
      </button>
    </div>
  );
}
