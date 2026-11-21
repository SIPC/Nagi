// 中文是基准：英文（../en）必须和这里的结构完全一致，少一条 tsc 就会报错。
// 每个命名空间对应一个页面或组件文件，通用的词放在 common。
import common from "./common";
import app from "./app";
import auth from "./auth";
import settings from "./settings";
import access from "./access";
import account from "./account";
import activity from "./activity";
import audit from "./audit";
import logs from "./logs";
import panelSecurity from "./panelSecurity";
import members from "./members";
import labels from "./labels";
import alerts from "./alerts";
import invite from "./invite";
import mesh from "./mesh";
import panels from "./panels";
import security from "./security";
import tokens from "./tokens";
import standby from "./standby";
import update from "./update";
import projects from "./projects";
import nodes from "./nodes";
import nodeAccess from "./nodeAccess";
import fileManager from "./fileManager";
import nodeImages from "./nodeImages";
import project from "./project";
import projectContainers from "./projectContainers";
import projectData from "./projectData";
import projectNetwork from "./projectNetwork";
import service from "./service";
import projectSettings from "./projectSettings";
import containerForm from "./containerForm";
import importDir from "./importDir";
import metrics from "./metrics";
import overview from "./overview";
import about from "./about";

export default {
  common,
  app,
  auth,
  settings,
  access,
  account,
  activity,
  audit,
  logs,
  panelSecurity,
  members,
  labels,
  alerts,
  invite,
  mesh,
  panels,
  security,
  tokens,
  standby,
  update,
  projects,
  nodes,
  nodeAccess,
  fileManager,
  nodeImages,
  project,
  projectContainers,
  projectData,
  projectNetwork,
  service,
  projectSettings,
  containerForm,
  importDir,
  metrics,
  overview,
  about,
};
