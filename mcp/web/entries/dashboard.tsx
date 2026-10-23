import { createRoot } from "react-dom/client";
import { Dashboard } from "../dashboard.tsx";
import { ToolShell } from "../shell.tsx";

const root = document.getElementById("root");
if (root) {
  createRoot(root).render(
    <ToolShell>
      <Dashboard />
    </ToolShell>,
  );
}
