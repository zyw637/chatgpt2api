import { useEffect } from "react";
import { Toaster } from "sonner";

import { AppShell } from "@/app/app-shell";

export default function App() {
  useEffect(() => {
    document.getElementById("pwa-boot")?.remove();
  }, []);

  return (
    <>
      <Toaster position="top-center" richColors offset={48} />
      <AppShell />
    </>
  );
}
