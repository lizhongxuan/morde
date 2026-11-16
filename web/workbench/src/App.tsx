import Workbench from './app/Workbench';
import { WorkbenchNoticeProvider } from './app/notice';

export default function App() {
  return <WorkbenchNoticeProvider><Workbench /></WorkbenchNoticeProvider>;
}
