import type { Messages } from "@/i18n";
import common from "./common";
import app from "./app";
import auth from "./auth";
import settings from "./settings";
import access from "./access";
import alerts from "./alerts";
import invite from "./invite";
import mesh from "./mesh";
import panels from "./panels";
import security from "./security";
import standby from "./standby";
import projects from "./projects";
import nodes from "./nodes";
import project from "./project";
import projectContainers from "./projectContainers";
import projectData from "./projectData";
import projectNetwork from "./projectNetwork";
import projectSettings from "./projectSettings";
import containerForm from "./containerForm";
import metrics from "./metrics";

export default {
  common,
  app,
  auth,
  settings,
  access,
  alerts,
  invite,
  mesh,
  panels,
  security,
  standby,
  projects,
  nodes,
  project,
  projectContainers,
  projectData,
  projectNetwork,
  projectSettings,
  containerForm,
  metrics,
} satisfies Messages;
