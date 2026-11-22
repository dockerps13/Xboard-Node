<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;

class ServerReportBatch extends Model
{
    protected $table = 'v2_server_report_batches';
    public $timestamps = false;
    protected $guarded = ['id'];
    protected $casts = [
        'server_id' => 'integer',
        'record_at' => 'integer',
        'applied_at' => 'integer',
        'created_at' => 'integer',
        'updated_at' => 'integer',
    ];
}
