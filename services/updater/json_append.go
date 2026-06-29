package updater

import "strconv"

func appendNullableString(buf []byte, v *string) []byte {
	if v == nil {
		return append(buf, `null`...)
	}
	return strconv.AppendQuote(buf, *v)
}

func appendNullableInt32OmitEmpty(buf []byte, name string, v *int32) []byte {
	if v == nil || *v == 0 {
		return buf
	}
	buf = append(buf, ',')
	buf = append(buf, name...)
	buf = append(buf, ':')
	return strconv.AppendInt(buf, int64(*v), 10)
}

func (r PrepareReply) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"ready":`...)
	buf = strconv.AppendBool(buf, r.Ready)
	buf = append(buf, `,"target":`...)
	buf = strconv.AppendQuote(buf, r.Target)
	buf = append(buf, `,"max_chunk_size":`...)
	buf = strconv.AppendUint(buf, uint64(r.MaxChunkSize), 10)
	buf = append(buf, '}')
	return buf
}

func (r PrepareReply) MarshalJSON() ([]byte, error) { return r.AppendJSON(make([]byte, 0, 80)), nil }

func (r CommitReply) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"accepted":`...)
	buf = strconv.AppendBool(buf, r.Accepted)
	if r.RebootRequired {
		buf = append(buf, `,"reboot_required":true`...)
	}
	buf = append(buf, '}')
	return buf
}

func (r CommitReply) MarshalJSON() ([]byte, error) { return r.AppendJSON(make([]byte, 0, 48)), nil }

func (r Reply) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"ok":`...)
	buf = strconv.AppendBool(buf, r.OK)
	if r.Accepted {
		buf = append(buf, `,"accepted":true`...)
	}
	if r.Error != "" {
		buf = append(buf, `,"error":`...)
		buf = strconv.AppendQuote(buf, r.Error)
	}
	buf = append(buf, '}')
	return buf
}

func (r Reply) MarshalJSON() ([]byte, error) { return r.AppendJSON(make([]byte, 0, 64)), nil }

func (f SoftwareFact) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"version":`...)
	buf = strconv.AppendQuote(buf, f.Version)
	buf = append(buf, `,"build_id":`...)
	buf = strconv.AppendQuote(buf, f.BuildID)
	buf = append(buf, `,"image_id":`...)
	buf = strconv.AppendQuote(buf, f.ImageID)
	buf = append(buf, `,"boot_id":`...)
	buf = strconv.AppendQuote(buf, f.BootID)
	if f.PayloadSHA256 != "" {
		buf = append(buf, `,"payload_sha256":`...)
		buf = strconv.AppendQuote(buf, f.PayloadSHA256)
	}
	buf = append(buf, '}')
	return buf
}

func (f SoftwareFact) MarshalJSON() ([]byte, error) { return f.AppendJSON(make([]byte, 0, 160)), nil }

func (f UpdaterFact) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"state":`...)
	buf = strconv.AppendQuote(buf, string(f.State))
	buf = append(buf, `,"last_error":`...)
	buf = appendNullableString(buf, f.LastError)
	buf = append(buf, `,"pending_version":`...)
	buf = appendNullableString(buf, f.PendingVersion)
	buf = append(buf, `,"pending_image_id":`...)
	buf = appendNullableString(buf, f.PendingImageID)
	buf = append(buf, `,"staged_image_id":`...)
	buf = appendNullableString(buf, f.StagedImageID)
	buf = append(buf, `,"job_id":`...)
	buf = appendNullableString(buf, f.JobID)
	buf = appendNullableInt32OmitEmpty(buf, `"boot_buy_rc"`, f.BootBuyRC)
	buf = append(buf, '}')
	return buf
}

func (f UpdaterFact) MarshalJSON() ([]byte, error) { return f.AppendJSON(make([]byte, 0, 192)), nil }

func (f HealthFact) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"state":`...)
	buf = strconv.AppendQuote(buf, f.State)
	if f.Reason != "" {
		buf = append(buf, `,"reason":`...)
		buf = strconv.AppendQuote(buf, f.Reason)
	}
	buf = append(buf, '}')
	return buf
}

func (f HealthFact) MarshalJSON() ([]byte, error) { return f.AppendJSON(make([]byte, 0, 64)), nil }

func (r StageReply) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"ok":`...)
	buf = strconv.AppendBool(buf, r.OK)
	if r.Err != "" {
		buf = append(buf, `,"err":`...)
		buf = strconv.AppendQuote(buf, r.Err)
	}
	if r.Stage != "" {
		buf = append(buf, `,"stage":`...)
		buf = strconv.AppendQuote(buf, r.Stage)
	}
	buf = append(buf, '}')
	return buf
}

func (r StageReply) MarshalJSON() ([]byte, error) { return r.AppendJSON(make([]byte, 0, 80)), nil }

func (d StagedDescriptor) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"version":`...)
	buf = strconv.AppendQuote(buf, d.Version)
	buf = append(buf, `,"build_id":`...)
	buf = strconv.AppendQuote(buf, d.BuildID)
	buf = append(buf, `,"image_id":`...)
	buf = strconv.AppendQuote(buf, d.ImageID)
	buf = append(buf, `,"length":`...)
	buf = strconv.AppendUint(buf, uint64(d.Length), 10)
	buf = append(buf, `,"slot":`...)
	buf = strconv.AppendUint(buf, uint64(d.Slot), 10)
	buf = append(buf, `,"payload_sha256":`...)
	buf = strconv.AppendQuote(buf, d.PayloadSHA256)
	buf = append(buf, '}')
	return buf
}

func (d StagedDescriptor) MarshalJSON() ([]byte, error) {
	return d.AppendJSON(make([]byte, 0, 192)), nil
}
