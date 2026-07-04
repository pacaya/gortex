void strlenCommand(client *c) {
    kvobj *kv;
    if ((kv = lookupKeyReadOrReply(c, c->argv[1], shared.czero)) == NULL ||
        checkType(c, kv, OBJ_STRING)) return;
    addReplyLongLong(c,stringObjectLen(kv));
}
